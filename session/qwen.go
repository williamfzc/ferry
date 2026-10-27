// This file reads and writes Qwen Code sessions. The format follows the
// MIT-licensed deja-vu reference parser (internal/sources/qwen.go): Qwen
// Code is a gemini-cli fork that stores JSONL files under
// ~/.qwen/projects/<encoded-project>/chats, using Claude-Code-style record
// envelopes with Gemini-style function call and response parts.
package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Qwen reads and writes the Qwen Code JSONL session format.
type Qwen struct{}

func (Qwen) Read(r io.Reader) (Session, error) { return readQwen(r) }

func (Qwen) Write(w io.Writer, s Session) error { return writeQwen(w, s) }

type qwenRecord struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"sessionId"`
	Timestamp  string          `json:"timestamp"`
	Cwd        string          `json:"cwd"`
	Message    json.RawMessage `json:"message"`
	ForkedFrom json.RawMessage `json:"forkedFrom"`
	Subtype    string          `json:"subtype"`
}

type qwenMessage struct {
	Role  string            `json:"role"`
	Parts []json.RawMessage `json:"parts"`
}

// readQwen parses one Qwen Code JSONL stream. Compression summaries are not
// expanded: their exact extraction depends on Qwen's resume trailer format,
// so they are counted and left in the original bytes instead of guessed.
func readQwen(r io.Reader) (Session, error) {
	s := Session{SourceType: "qwen"}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 32*1024*1024)
	line := 0
	recognized := false
	for scan.Scan() {
		line++
		var record qwenRecord
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			return s, fmt.Errorf("%s:%d: invalid qwen record: %w", s.SourcePath, line, err)
		}
		if record.SessionID != "" && s.NativeID == "" {
			s.NativeID = record.SessionID
		}
		if record.Cwd != "" && s.Workspace == "" {
			s.Workspace = record.Cwd
		}
		switch record.Type {
		case "user", "assistant", "tool_result", "system":
			recognized = true
		default:
			// Unknown future record types stay out of the transcript and are
			// reported by skippedQwenLimitations.
			continue
		}
		// A forkedFrom marker only labels the record as copied from a
		// /branch source; the copied records are this file's own history and
		// stay in the transcript (counted by skippedQwenLimitations).
		if record.Type == "system" {
			// chat_compression summaries need Qwen's exact resume trailer
			// rules; the original bytes retain them and the limitation
			// reports them.
			continue
		}
		var envelope qwenMessage
		if err := json.Unmarshal(record.Message, &envelope); err != nil {
			return s, fmt.Errorf("%s:%d: invalid qwen message: %w", s.SourcePath, line, err)
		}
		role := envelope.Role
		if role == "model" {
			role = "assistant"
		}
		var parts []ContentBlock
		for _, rawPart := range envelope.Parts {
			var fields struct {
				Text             string          `json:"text"`
				Thought          bool            `json:"thought"`
				FunctionCall     json.RawMessage `json:"functionCall"`
				FunctionResponse json.RawMessage `json:"functionResponse"`
			}
			if err := json.Unmarshal(rawPart, &fields); err != nil {
				return s, fmt.Errorf("%s:%d: invalid qwen part: %w", s.SourcePath, line, err)
			}
			switch {
			case len(fields.FunctionCall) > 0 && !isJSONNull(fields.FunctionCall):
				var call struct {
					ID   string          `json:"id"`
					Name string          `json:"name"`
					Args json.RawMessage `json:"args"`
				}
				if err := json.Unmarshal(fields.FunctionCall, &call); err != nil {
					return s, fmt.Errorf("%s:%d: invalid qwen functionCall: %w", s.SourcePath, line, err)
				}
				if call.ID == "" || call.Name == "" {
					return s, fmt.Errorf("%s:%d: qwen functionCall is missing id or name", s.SourcePath, line)
				}
				if len(call.Args) == 0 || isJSONNull(call.Args) {
					call.Args = json.RawMessage(`{}`)
				}
				parts = append(parts, ContentBlock{Type: "tool_call", ToolCall: &ToolCall{ID: call.ID, Name: call.Name, Input: bytes.Clone(call.Args)}, Raw: bytes.Clone(rawPart)})
			case len(fields.FunctionResponse) > 0 && !isJSONNull(fields.FunctionResponse):
				var response struct {
					ID       string `json:"id"`
					Name     string `json:"name"`
					Response struct {
						Output json.RawMessage `json:"output"`
						Error  json.RawMessage `json:"error"`
					} `json:"response"`
				}
				if err := json.Unmarshal(fields.FunctionResponse, &response); err != nil {
					return s, fmt.Errorf("%s:%d: invalid qwen functionResponse: %w", s.SourcePath, line, err)
				}
				if response.ID == "" {
					return s, fmt.Errorf("%s:%d: qwen functionResponse has no id", s.SourcePath, line)
				}
				result := &ToolResult{ID: response.ID}
				if len(response.Response.Error) > 0 && !isJSONNull(response.Response.Error) {
					result.IsError = true
				}
				output := response.Response.Output
				hasError := len(response.Response.Error) > 0 && !isJSONNull(response.Response.Error)
				var plain string
				if len(output) == 0 || isJSONNull(output) {
					// A failure with no output still carries its reason; the
					// error text is the only record of what happened.
					if hasError {
						result.Content = text(response.Response.Error)
					}
				} else if json.Unmarshal(output, &plain) == nil {
					result.Content = plain
				} else {
					result.Content = text(output)
					result.RawContent = bytes.Clone(output)
				}
				parts = append(parts, ContentBlock{Type: "tool_result", ToolResult: result, Raw: bytes.Clone(rawPart)})
			case fields.Text != "":
				if fields.Thought {
					// Model reasoning: reported by skippedQwenLimitations,
					// never exposed through the projection.
					continue
				}
				parts = append(parts, ContentBlock{Type: "text", Text: fields.Text, Raw: bytes.Clone(rawPart)})
			}
		}
		if len(parts) > 0 {
			s.Messages = append(s.Messages, Message{Role: role, Timestamp: record.Timestamp, Parts: parts})
		}
	}
	if err := scan.Err(); err != nil {
		return s, err
	}
	if !recognized {
		return s, fmt.Errorf("%s: no recognizable qwen records", s.SourcePath)
	}
	if s.NativeID == "" {
		return s, fmt.Errorf("%s: qwen records carry no sessionId", s.SourcePath)
	}
	return s, nil
}

// writeQwen emits the reference Qwen record subset. Tool results travel as
// their own tool_result records carrying a functionResponse part, which
// requires the tool name from the earlier call.
func writeQwen(w io.Writer, s Session) error {
	if s.NativeID == "" {
		return fmt.Errorf("qwen native ID is required")
	}
	if !isUUID(s.NativeID) {
		return fmt.Errorf("Qwen native ID must be a UUID")
	}
	if err := validateMessages(s, "qwen"); err != nil {
		return err
	}
	toolNames := make(map[string]string)
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_call" && part.ToolCall != nil {
				toolNames[part.ToolCall.ID] = qualifiedToolName(part.ToolCall)
			}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for i, message := range s.Messages {
		if message.Role == "summary" {
			return fmt.Errorf("message %d: summary cannot be represented by Qwen", i)
		}
		if message.Role != "user" && message.Role != "assistant" && message.Role != "tool" {
			return fmt.Errorf("message %d: unsupported role %q", i, message.Role)
		}
		if len(message.Parts) == 0 {
			return fmt.Errorf("message %d: empty message", i)
		}
		recordType := message.Role
		envelopeRole := message.Role
		if message.Role == "assistant" {
			envelopeRole = "model"
		}
		if message.Role == "tool" {
			recordType = "tool_result"
			envelopeRole = "user"
		}
		parts := make([]map[string]any, 0, len(message.Parts))
		for j, part := range message.Parts {
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"text": part.Text})
			case "tool_call":
				object, ok := decodeObjectInput(part.ToolCall.Input)
				if !ok {
					return fmt.Errorf("message %d part %d: Qwen tool arguments must be a JSON object", i, j)
				}
				parts = append(parts, map[string]any{"functionCall": map[string]any{
					"id":   part.ToolCall.ID,
					"name": qualifiedToolName(part.ToolCall),
					"args": json.RawMessage(object),
				}})
			case "tool_result":
				name, ok := toolNames[part.ToolResult.ID]
				if !ok {
					return fmt.Errorf("message %d part %d: tool result %q has no preceding tool call", i, j, part.ToolResult.ID)
				}
				content, err := toolResultText(part.ToolResult)
				if err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				response := map[string]any{"output": content}
				if part.ToolResult.IsError {
					response["error"] = "tool reported an error"
				}
				parts = append(parts, map[string]any{"functionResponse": map[string]any{
					"id":       part.ToolResult.ID,
					"name":     name,
					"response": response,
				}})
			default:
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, part.Type)
			}
		}
		envelope := map[string]any{"role": envelopeRole, "parts": parts}
		record := map[string]any{"type": recordType, "sessionId": s.NativeID, "timestamp": message.Timestamp, "message": envelope}
		if s.Workspace != "" {
			// Qwen records carry the working directory; without it the
			// read-back identity check cannot recover the workspace.
			record["cwd"] = s.Workspace
		}
		if err := enc.Encode(record); err != nil {
			return err
		}
	}
	_, err := io.Copy(w, &buf)
	return err
}

// parseQwenFile reads one local Qwen Code JSONL file.
func parseQwenFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	s := Session{SourceType: "qwen", SourcePath: path}
	if err != nil {
		return s, err
	}
	s, err = readQwen(bytes.NewReader(data))
	if err != nil {
		s.SourcePath = path
		return s, err
	}
	s.Original = data
	s.SourcePath = path
	if info, statErr := os.Stat(path); statErr == nil {
		s.Updated = info.ModTime()
	}
	sum := sha256.Sum256([]byte("qwen\x00" + path))
	s.ID = hex.EncodeToString(sum[:16])
	return s, nil
}

// isQwenChatFile restricts discovery to the chats directories, mirroring the
// reference parser: projects/<project>/chats/<id>.jsonl, plus sub-agent logs
// under projects/<project>/subagents/<session>/agent-<id>.jsonl.
func isQwenChatFile(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := splitPath(rel)
	if len(parts) == 3 && parts[1] == "chats" && filepath.Ext(path) == ".jsonl" {
		return true
	}
	return len(parts) == 4 && parts[1] == "subagents" && stringsHasPrefix(parts[2], "") && stringsHasPrefix(filepath.Base(path), "agent-")
}

func splitPath(rel string) []string {
	return strings.Split(rel, string(filepath.Separator))
}

func stringsHasPrefix(s, prefix string) bool {
	return strings.HasPrefix(s, prefix)
}

// skippedQwenLimitations reports the Qwen content that stays out of the
// common projection: model reasoning, compression summaries (not expanded
// because their exact extraction depends on Qwen's resume trailer rules),
// branch forks, and unknown record types.
func skippedQwenLimitations(s Session) []string {
	if s.SourceType != "qwen" {
		return nil
	}
	thoughts := 0
	compressions := 0
	forks := 0
	unknown := make(map[string]int)
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var record qwenRecord
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		switch record.Type {
		case "user", "assistant":
			var envelope qwenMessage
			if json.Unmarshal(record.Message, &envelope) == nil {
				for _, rawPart := range envelope.Parts {
					var fields struct {
						Text    string `json:"text"`
						Thought bool   `json:"thought"`
					}
					if json.Unmarshal(rawPart, &fields) == nil && fields.Thought && fields.Text != "" {
						thoughts++
					}
				}
			}
		case "system":
			if record.Subtype == "chat_compression" {
				compressions++
			} else {
				name := record.Subtype
				if name == "" {
					name = "<missing subtype>"
				}
				unknown["system/"+name]++
			}
		default:
			name := record.Type
			if name == "" {
				name = "<missing type>"
			}
			unknown[name]++
		}
		if len(record.ForkedFrom) > 0 && !isJSONNull(record.ForkedFrom) {
			forks++
		}
	}
	limitations := make([]string, 0, 4)
	if thoughts > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Qwen thought block(s) that have no cross-provider part; the original bytes retain them.", thoughts))
	}
	if compressions > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Qwen compression record(s) whose summaries are not expanded; the original bytes retain them.", compressions))
	}
	if forks > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Qwen /branch fork marker(s) on copied records; the copied history stays in the projection.", forks))
	}
	if len(unknown) > 0 {
		types := make([]string, 0, len(unknown))
		for name := range unknown {
			types = append(types, name)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, name := range types {
			parts = append(parts, fmt.Sprintf("%s=%d", name, unknown[name]))
		}
		limitations = append(limitations, fmt.Sprintf("Source contains unrepresented Qwen record types (%s); the original bytes retain them.", strings.Join(parts, ", ")))
	}
	return limitations
}
