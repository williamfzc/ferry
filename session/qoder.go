// This file reads and writes Qoder CLI sessions. The format was captured
// from local Qoder CLI data: one JSONL file per session under
// ~/.qoder/projects/<flattened-path> with a parent-linked record chain,
// epoch-millisecond timestamps, and an optional <session-id>-session.json
// sidecar carrying the workspace and title.
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
	"sort"
	"strconv"
	"strings"
	"time"
)

// Qoder reads and writes the Qoder CLI JSONL session format.
type Qoder struct{}

func (Qoder) Read(r io.Reader) (Session, error) { return readQoder(r) }

func (Qoder) Write(w io.Writer, s Session) error { return writeQoder(w, s) }

type qoderRecord struct {
	ID        string          `json:"id"`
	SessionID string          `json:"session_id"`
	ParentID  string          `json:"parent_id"`
	Role      string          `json:"role"`
	Parts     []qoderPart     `json:"parts"`
	CreatedAt int64           `json:"created_at"`
	Model     string          `json:"model"`
	Provider  string          `json:"provider"`
	Usage     json.RawMessage `json:"usage"`
	IsMeta    bool            `json:"is_meta"`
}

type qoderPart struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type qoderToolCallData struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Input    string `json:"input"`
	Finished bool   `json:"finished"`
}

type qoderToolResultData struct {
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	// Metadata carries tool-execution metadata with no cross-provider
	// equivalent. It stays in the original bytes only.
	Metadata json.RawMessage `json:"metadata"`
}

// readQoder parses one Qoder JSONL stream. The records form a parent-linked
// chain, so a record whose parent is not the previous record means the
// history branched or was replaced; that cannot be represented faithfully
// and fails closed.
func readQoder(r io.Reader) (Session, error) {
	s := Session{SourceType: "qoder"}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 32*1024*1024)
	line := 0
	recognized := false
	previousID := ""
	first := true
	for scan.Scan() {
		line++
		var record qoderRecord
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			return s, fmt.Errorf("%s:%d: invalid qoder record: %w", s.SourcePath, line, err)
		}
		if record.ID == "" || record.SessionID == "" || record.Role == "" {
			return s, fmt.Errorf("%s:%d: qoder record is missing id, session_id, or role", s.SourcePath, line)
		}
		recognized = true
		if s.NativeID == "" {
			s.NativeID = record.SessionID
		}
		if first {
			if record.ParentID != "" {
				return s, fmt.Errorf("%s:%d: first qoder record has parent %q; the chain must start empty", s.SourcePath, line, record.ParentID)
			}
			first = false
		} else if record.ParentID != previousID {
			return s, fmt.Errorf("%s:%d: qoder parent chain branches (parent %q, previous %q); branch reconstruction is not supported", s.SourcePath, line, record.ParentID, previousID)
		}
		previousID = record.ID
		if record.CreatedAt == 0 {
			return s, fmt.Errorf("%s:%d: qoder record has no created_at timestamp", s.SourcePath, line)
		}
		if record.IsMeta {
			// Injected provider context. The original bytes retain it and
			// skippedQoderLimitations reports it.
			continue
		}
		message := Message{Role: record.Role, Timestamp: qoderTimestamp(record.CreatedAt)}
		for _, part := range record.Parts {
			switch part.Type {
			case "text":
				var data struct {
					Type string `json:"type"`
					Text string `json:"text"`
				}
				if err := json.Unmarshal(part.Data, &data); err != nil {
					return s, fmt.Errorf("%s:%d: invalid qoder text part: %w", s.SourcePath, line, err)
				}
				if data.Text == "" {
					continue
				}
				message.Parts = append(message.Parts, ContentBlock{Type: "text", Text: data.Text, Raw: bytes.Clone(part.Data)})
			case "tool_call":
				var data qoderToolCallData
				if err := json.Unmarshal(part.Data, &data); err != nil {
					return s, fmt.Errorf("%s:%d: invalid qoder tool_call part: %w", s.SourcePath, line, err)
				}
				if data.ID == "" || data.Name == "" {
					return s, fmt.Errorf("%s:%d: qoder tool_call is missing id or name", s.SourcePath, line)
				}
				call := &ToolCall{ID: data.ID, Name: data.Name, Input: qoderInput(data.Input)}
				if !data.Finished {
					// An unfinished call must not be mistaken for a completed one.
					call.Status = "in_progress"
				}
				message.Parts = append(message.Parts, ContentBlock{Type: "tool_call", ToolCall: call, Raw: bytes.Clone(part.Data)})
			case "tool_result":
				var data qoderToolResultData
				if err := json.Unmarshal(part.Data, &data); err != nil {
					return s, fmt.Errorf("%s:%d: invalid qoder tool_result part: %w", s.SourcePath, line, err)
				}
				if data.ToolUseID == "" {
					return s, fmt.Errorf("%s:%d: qoder tool_result has no tool_use_id", s.SourcePath, line)
				}
				result := &ToolResult{ID: data.ToolUseID, IsError: data.IsError}
				var plain string
				if isJSONNull(data.Content) {
					result.Content = "null"
				} else if json.Unmarshal(data.Content, &plain) == nil {
					result.Content = plain
				} else {
					result.Content = text(data.Content)
					result.RawContent = bytes.Clone(data.Content)
				}
				message.Parts = append(message.Parts, ContentBlock{Type: "tool_result", ToolResult: result, Raw: bytes.Clone(part.Data)})
			case "finish":
				// Terminal-state metadata (stop reason and wall time) on the
				// assistant record. No cross-provider equivalent; the original
				// bytes retain it.
			default:
				message.Parts = append(message.Parts, ContentBlock{Type: "unknown", Raw: bytes.Clone(part.Data)})
			}
		}
		if len(message.Parts) > 0 {
			s.Messages = append(s.Messages, message)
		}
	}
	if err := scan.Err(); err != nil {
		return s, err
	}
	if !recognized {
		return s, fmt.Errorf("%s: no recognizable qoder records", s.SourcePath)
	}
	return s, nil
}

// qoderInput decodes Qoder's JSON-object-as-string tool input. An empty
// input is an empty object.
func qoderInput(encoded string) json.RawMessage {
	if encoded == "" {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(encoded)
}

func qoderTimestamp(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z07:00")
}

func qoderMillis(timestamp string) (int64, error) {
	parsed, err := time.Parse(time.RFC3339Nano, timestamp)
	if err != nil {
		return 0, fmt.Errorf("invalid timestamp %q: %w", timestamp, err)
	}
	return parsed.UnixMilli(), nil
}

// writeQoder emits the observed Qoder record subset. The JSONL cannot carry
// the workspace (it lives in the optional sidecar file), so no workspace is
// required; everything else validates before the first byte is written.
func writeQoder(w io.Writer, s Session) error {
	if s.NativeID == "" {
		return fmt.Errorf("qoder native ID is required")
	}
	if !isUUID(s.NativeID) {
		return fmt.Errorf("Qoder native ID must be a UUID")
	}
	if err := validateMessages(s, "qoder"); err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	previousID := ""
	lastMillis := int64(0)
	haveMillis := false
	for i, message := range s.Messages {
		if message.Role == "summary" {
			return fmt.Errorf("message %d: summary cannot be represented by Qoder", i)
		}
		millis := lastMillis
		if message.Timestamp != "" {
			parsed, err := qoderMillis(message.Timestamp)
			if err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			millis = parsed
			lastMillis = millis
			haveMillis = true
		} else if !haveMillis {
			return fmt.Errorf("message 0: Qoder record timestamp is required")
		}
		recordID := ferryUUID("qoder", s.NativeID+"\x00"+strconv.Itoa(i)+"\x00"+message.Role)
		parts := make([]map[string]any, 0, len(message.Parts))
		for j, part := range message.Parts {
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"type": "text", "data": map[string]any{"type": "text", "text": part.Text}})
			case "tool_call":
				object, ok := decodeObjectInput(part.ToolCall.Input)
				if !ok {
					return fmt.Errorf("message %d part %d: Qoder tool input must be a JSON object", i, j)
				}
				data := map[string]any{"type": "function", "id": part.ToolCall.ID, "name": qualifiedToolName(part.ToolCall), "input": string(object), "finished": true}
				if part.ToolCall.Status == "in_progress" {
					data["finished"] = false
				}
				parts = append(parts, map[string]any{"type": "tool_call", "data": data})
			case "tool_result":
				content, err := toolResultText(part.ToolResult)
				if err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				data := map[string]any{"tool_use_id": part.ToolResult.ID, "content": content}
				if part.ToolResult.IsError {
					data["is_error"] = true
				}
				parts = append(parts, map[string]any{"type": "tool_result", "data": data})
			default:
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, part.Type)
			}
		}
		if len(parts) == 0 {
			return fmt.Errorf("message %d: empty message", i)
		}
		record := map[string]any{
			"id":          recordID,
			"session_id":  s.NativeID,
			"parent_id":   previousID,
			"role":        message.Role,
			"parts":       parts,
			"model":       "",
			"provider":    "",
			"created_at":  millis,
			"updated_at":  millis,
			"finished_at": millis,
			"is_meta":     false,
		}
		if err := enc.Encode(record); err != nil {
			return err
		}
		previousID = recordID
	}
	_, err := io.Copy(w, &buf)
	return err
}

// parseQoderFile reads one local Qoder session: the JSONL stream plus the
// optional sidecar that carries the workspace. The sidecar is auxiliary; a
// missing or unreadable sidecar only means the workspace stays empty.
func parseQoderFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	s := Session{SourceType: "qoder", SourcePath: path}
	if err != nil {
		return s, err
	}
	s, err = readQoder(bytes.NewReader(data))
	if err != nil {
		s.SourcePath = path
		return s, err
	}
	s.Original = data
	s.SourcePath = path
	if sidecar, err := os.ReadFile(strings.TrimSuffix(path, ".jsonl") + "-session.json"); err == nil {
		var meta struct {
			WorkingDir string `json:"working_dir"`
		}
		if json.Unmarshal(sidecar, &meta) == nil {
			s.Workspace = meta.WorkingDir
		}
	}
	sum := sha256.Sum256([]byte("qoder\x00" + path))
	s.ID = hex.EncodeToString(sum[:16])
	return s, nil
}

// skippedQoderLimitations reports the Qoder content that stays out of the
// common projection: injected meta messages and unknown part types.
func skippedQoderLimitations(s Session) []string {
	if s.SourceType != "qoder" {
		return nil
	}
	metaMessages := 0
	unknownParts := make(map[string]int)
	var lines [][]byte
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			lines = append(lines, line)
		}
	}
	for _, line := range lines {
		var record qoderRecord
		if json.Unmarshal(line, &record) != nil {
			continue
		}
		if record.IsMeta {
			metaMessages++
			continue
		}
		for _, part := range record.Parts {
			switch part.Type {
			case "text", "tool_call", "tool_result", "finish":
			default:
				// Unknown parts also stay in Messages as unknown blocks, so
				// Convert fails closed before this report can list them;
				// the counter documents them for source-side inspection.
				name := part.Type
				if name == "" {
					name = "<missing type>"
				}
				unknownParts[name]++
			}
		}
	}
	limitations := make([]string, 0, 2)
	if metaMessages > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Qoder meta message(s) injected as provider context; the original bytes retain them.", metaMessages))
	}
	if len(unknownParts) > 0 {
		types := make([]string, 0, len(unknownParts))
		for name := range unknownParts {
			types = append(types, name)
		}
		sort.Strings(types)
		parts := make([]string, 0, len(types))
		for _, name := range types {
			parts = append(parts, fmt.Sprintf("%s=%d", name, unknownParts[name]))
		}
		limitations = append(limitations, fmt.Sprintf("Source contains unrepresented Qoder part types (%s); the original bytes retain them.", strings.Join(parts, ", ")))
	}
	return limitations
}
