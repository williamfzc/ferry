// This file reads and writes GitHub Copilot CLI sessions. The format follows
// the MIT-licensed deja-vu reference parser (internal/sources/copilot.go):
// each session is an append-only events.jsonl under
// ~/.copilot/session-state/<uuid>, pairing tool.execution_start and
// tool.execution_complete events by call id.
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
	"regexp"
	"sort"
	"strings"
)

// Copilot reads and writes the GitHub Copilot CLI JSONL event format.
type Copilot struct{}

func (Copilot) Read(r io.Reader) (Session, error) { return readCopilot(r) }

func (Copilot) Write(w io.Writer, s Session) error { return writeCopilot(w, s) }

type copilotEvent struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	AgentID   string          `json:"agentId"`
	Data      json.RawMessage `json:"data"`
}

// copilotSkillContextRe strips Copilot's injected skill context from user
// text so provider-spliced instructions are not mistaken for user input.
var copilotSkillContextRe = regexp.MustCompile(`(?s)<skill-context[^>]*>.*?</skill-context>`)

// copilotArguments accepts a JSON object or, for GPT-model apply_patch
// calls (deja-vu #4491), a freeform patch string that stays a JSON string so
// the free-text wrapping rules apply downstream.
func copilotArguments(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return json.RawMessage(`{}`), nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err == nil && object != nil {
		return raw, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return raw, nil
	}
	return nil, fmt.Errorf("copilot tool arguments must be a JSON object or a string")
}

// readCopilot parses one Copilot CLI event stream.
func readCopilot(r io.Reader) (Session, error) {
	s := Session{SourceType: "copilot"}
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 4096), 32*1024*1024)
	line := 0
	recognized := false
	for scan.Scan() {
		line++
		var event copilotEvent
		if err := json.Unmarshal(scan.Bytes(), &event); err != nil {
			return s, fmt.Errorf("%s:%d: invalid copilot event: %w", s.SourcePath, line, err)
		}
		if event.AgentID != "" {
			// Sub-agent sidechain events belong to a child session; the main
			// transcript must not absorb them.
			continue
		}
		switch event.Type {
		case "session.start":
			recognized = true
			var data struct {
				SessionID string `json:"sessionId"`
				StartTime string `json:"startTime"`
				Context   struct {
					Cwd string `json:"cwd"`
				} `json:"context"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid session.start data: %w", s.SourcePath, line, err)
			}
			if s.NativeID == "" && data.SessionID != "" {
				s.NativeID = data.SessionID
			}
			if s.Workspace == "" {
				s.Workspace = data.Context.Cwd
			}
		case "session.info", "session.shutdown", "session.resume":
			recognized = true
			// Lifecycle metadata with no cross-provider meaning.
		case "user.message", "assistant.message":
			recognized = true
			var data struct {
				Content string `json:"content"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid %s data: %w", s.SourcePath, line, event.Type, err)
			}
			content := data.Content
			if event.Type == "user.message" {
				// The reference strips injected skill context from user
				// messages only; assistant text passes through untouched.
				content = copilotSkillContextRe.ReplaceAllString(content, "")
				if strings.TrimSpace(content) == "" {
					continue
				}
			}
			role := "user"
			if event.Type == "assistant.message" {
				role = "assistant"
			}
			s.Messages = append(s.Messages, Message{Role: role, Timestamp: event.Timestamp, Parts: []ContentBlock{{Type: "text", Text: content}}})
		case "tool.execution_start":
			recognized = true
			var data struct {
				ToolName   string          `json:"toolName"`
				ToolCallID string          `json:"toolCallId"`
				Arguments  json.RawMessage `json:"arguments"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid tool.execution_start data: %w", s.SourcePath, line, err)
			}
			if data.ToolCallID == "" || data.ToolName == "" {
				return s, fmt.Errorf("%s:%d: copilot tool.execution_start is missing toolCallId or toolName", s.SourcePath, line)
			}
			input, err := copilotArguments(data.Arguments)
			if err != nil {
				return s, fmt.Errorf("%s:%d: %w", s.SourcePath, line, err)
			}
			call := &ToolCall{ID: data.ToolCallID, Name: data.ToolName, Input: input}
			s.Messages = append(s.Messages, Message{Role: "assistant", Timestamp: event.Timestamp, Parts: []ContentBlock{{Type: "tool_call", ToolCall: call}}})
		case "tool.execution_complete":
			recognized = true
			var data struct {
				ToolCallID string `json:"toolCallId"`
				Result     struct {
					Content string `json:"content"`
				} `json:"result"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid tool.execution_complete data: %w", s.SourcePath, line, err)
			}
			if data.ToolCallID == "" {
				// Without a call id the result cannot pair with any call;
				// it is reported instead of being attached to a guess.
				continue
			}
			result := &ToolResult{ID: data.ToolCallID, Content: data.Result.Content}
			s.Messages = append(s.Messages, Message{Role: "tool", Timestamp: event.Timestamp, Parts: []ContentBlock{{Type: "tool_result", ToolResult: result}}})
		case "session.compaction_complete":
			recognized = true
			var data struct {
				Success        bool   `json:"success"`
				SummaryContent string `json:"summaryContent"`
			}
			if err := json.Unmarshal(event.Data, &data); err != nil {
				return s, fmt.Errorf("%s:%d: invalid session.compaction_complete data: %w", s.SourcePath, line, err)
			}
			if !data.Success || data.SummaryContent == "" {
				continue
			}
			s.Messages = append(s.Messages, Message{Role: "summary", Timestamp: event.Timestamp, Parts: []ContentBlock{{Type: "text", Text: data.SummaryContent}}})
		default:
			// Unknown future events (including subagent.* records) stay out
			// of the transcript and are reported by skippedCopilotLimitations.
		}
	}
	if err := scan.Err(); err != nil {
		return s, err
	}
	if !recognized {
		return s, fmt.Errorf("%s: no recognizable copilot records", s.SourcePath)
	}
	return s, nil
}

// writeCopilot emits the reference Copilot event subset. Every conversion
// starts from a session.start so the target can identify the session and its
// workspace.
func writeCopilot(w io.Writer, s Session) error {
	if s.NativeID == "" {
		return fmt.Errorf("copilot native ID is required")
	}
	if !isUUID(s.NativeID) {
		return fmt.Errorf("Copilot native ID must be a UUID")
	}
	if !filepath.IsAbs(s.Workspace) {
		return fmt.Errorf("Copilot workspace must be an absolute path")
	}
	if err := validateMessages(s, "copilot"); err != nil {
		return err
	}
	headerTimestamp := sessionTimestamp(s)
	if headerTimestamp == "" {
		return fmt.Errorf("Copilot session timestamp is required")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	start := map[string]any{
		"sessionId": s.NativeID,
		"startTime": headerTimestamp,
		"context":   map[string]any{"cwd": s.Workspace},
	}
	if err := enc.Encode(map[string]any{"type": "session.start", "timestamp": headerTimestamp, "data": start}); err != nil {
		return err
	}
	emit := func(eventType, timestamp string, data map[string]any) error {
		if timestamp == "" {
			timestamp = headerTimestamp
		}
		return enc.Encode(map[string]any{"type": eventType, "timestamp": timestamp, "data": data})
	}
	for i, message := range s.Messages {
		if message.Role == "summary" {
			if err := validateSummary(message); err != nil {
				return fmt.Errorf("message %d: %w", i, err)
			}
			for _, part := range message.Parts {
				if err := emit("session.compaction_complete", message.Timestamp, map[string]any{"success": true, "summaryContent": part.Text}); err != nil {
					return err
				}
			}
			continue
		}
		for j, part := range message.Parts {
			switch part.Type {
			case "text":
				if message.Role == "user" {
					if err := emit("user.message", message.Timestamp, map[string]any{"content": part.Text}); err != nil {
						return err
					}
					continue
				}
				if message.Role == "assistant" {
					if err := emit("assistant.message", message.Timestamp, map[string]any{"content": part.Text}); err != nil {
						return err
					}
					continue
				}
				return fmt.Errorf("message %d part %d: tool text needs a tool result", i, j)
			case "tool_call":
				object, ok := decodeObjectInput(part.ToolCall.Input)
				if !ok {
					// Reads keep apply_patch's freeform patch string, but a
					// written event needs the object form; free text is
					// wrapped and reported by Convert before it gets here.
					return fmt.Errorf("message %d part %d: Copilot tool arguments must be a JSON object", i, j)
				}
				data := map[string]any{"toolName": qualifiedToolName(part.ToolCall), "toolCallId": part.ToolCall.ID, "arguments": json.RawMessage(object)}
				if err := emit("tool.execution_start", message.Timestamp, data); err != nil {
					return err
				}
			case "tool_result":
				content, err := toolResultText(part.ToolResult)
				if err != nil {
					return fmt.Errorf("message %d part %d: %w", i, j, err)
				}
				data := map[string]any{"toolCallId": part.ToolResult.ID, "result": map[string]any{"content": content}}
				if err := emit("tool.execution_complete", message.Timestamp, data); err != nil {
					return err
				}
			default:
				return fmt.Errorf("message %d part %d: unsupported content %q", i, j, part.Type)
			}
		}
	}
	_, err := io.Copy(w, &buf)
	return err
}

// parseCopilotFile reads one local Copilot CLI events.jsonl file.
func parseCopilotFile(path string) (Session, error) {
	data, err := os.ReadFile(path)
	s := Session{SourceType: "copilot", SourcePath: path}
	if err != nil {
		return s, err
	}
	s, err = readCopilot(bytes.NewReader(data))
	if err != nil {
		s.SourcePath = path
		return s, err
	}
	s.Original = data
	s.SourcePath = path
	if info, statErr := os.Stat(path); statErr == nil {
		s.Updated = info.ModTime()
	}
	sum := sha256.Sum256([]byte("copilot\x00" + path))
	s.ID = hex.EncodeToString(sum[:16])
	return s, nil
}

// skippedCopilotLimitations reports the Copilot content that stays out of
// the common projection: injected skill context, sub-agent sidechains, and
// unknown event types.
func skippedCopilotLimitations(s Session) []string {
	if s.SourceType != "copilot" {
		return nil
	}
	skillContext := 0
	sidechain := 0
	resultsWithoutCall := 0
	unknown := make(map[string]int)
	for _, line := range bytes.Split(s.Original, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event copilotEvent
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if event.AgentID != "" {
			sidechain++
			continue
		}
		switch event.Type {
		case "user.message", "assistant.message":
			var data struct {
				Content string `json:"content"`
			}
			if json.Unmarshal(event.Data, &data) == nil {
				skillContext += len(copilotSkillContextRe.FindAllString(data.Content, -1))
			}
		case "tool.execution_complete":
			var data struct {
				ToolCallID string `json:"toolCallId"`
			}
			if json.Unmarshal(event.Data, &data) == nil && data.ToolCallID == "" {
				resultsWithoutCall++
			}
		case "session.start", "session.info", "session.shutdown", "session.resume", "tool.execution_start", "session.compaction_complete":
		default:
			name := event.Type
			if name == "" {
				name = "<missing type>"
			}
			unknown[name]++
		}
	}
	limitations := make([]string, 0, 3)
	if skillContext > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Copilot skill-context block(s) injected into user messages; the original bytes retain them.", skillContext))
	}
	if sidechain > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d Copilot sub-agent sidechain event(s); the original bytes retain them.", sidechain))
	}
	if resultsWithoutCall > 0 {
		limitations = append(limitations, fmt.Sprintf("Source contains %d tool result(s) with no toolCallId; they cannot pair with any call.", resultsWithoutCall))
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
		limitations = append(limitations, fmt.Sprintf("Source contains unrepresented Copilot event types (%s); the original bytes retain them.", strings.Join(parts, ", ")))
	}
	return limitations
}
