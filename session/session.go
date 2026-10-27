// This file reads supported agent histories into the session model.
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
	"strings"
)

type record struct {
	Type      string          `json:"type"`
	Timestamp string          `json:"timestamp"`
	SessionID string          `json:"sessionId"`
	Cwd       string          `json:"cwd"`
	Payload   json.RawMessage `json:"payload"`
	Message   json.RawMessage `json:"message"`
}
type body struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
	Cwd       string          `json:"cwd"`
	SessionID string          `json:"session_id"`
	Message   string          `json:"message"`
	Status    string          `json:"status"`
	// IsError is not part of Codex's observed rollout format, but earlier
	// development snapshots wrote it; reading it keeps those files honest.
	IsError bool `json:"is_error"`
}
type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
	Status    string          `json:"status"`
}

func text(raw json.RawMessage) string {
	// json.Unmarshal accepts JSON null into a string and leaves the zero value
	// behind. That makes an explicit null tool result indistinguishable from
	// an empty string. Keep the JSON spelling in the common text projection;
	// RawContent still retains the original value for structured targets.
	if isJSONNull(raw) {
		return "null"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// contentParts preserves every content block, including unfamiliar future types.
func contentParts(raw json.RawMessage) []ContentBlock {
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return []ContentBlock{{Type: "text", Text: str, Raw: raw}}
	}
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		if len(raw) > 0 && string(raw) != "null" {
			return []ContentBlock{{Type: "unknown", Raw: raw}}
		}
		return nil
	}
	parts := make([]ContentBlock, 0, len(items))
	for _, item := range items {
		var b block
		if json.Unmarshal(item, &b) != nil {
			parts = append(parts, ContentBlock{Type: "unknown", Raw: item})
			continue
		}
		p := ContentBlock{Type: b.Type, Raw: item}
		switch b.Type {
		case "text", "input_text", "output_text":
			p.Type = "text"
			p.Text = b.Text
		case "thinking":
			// Model reasoning has no cross-provider part; the limitation
			// reporting counts it from the original bytes.
			continue
		case "tool_use":
			p.Type = "tool_call"
			p.ToolCall = &ToolCall{ID: b.ID, Name: b.Name, Input: b.Input, Status: acpToolCallStatus(b.Status)}
		case "tool_result":
			p.ToolResult = &ToolResult{ID: b.ToolUseID, Content: text(b.Content), IsError: b.IsError, RawContent: b.Content}
		}
		if p.Type == "" {
			p.Type = "unknown"
		}
		parts = append(parts, p)
	}
	return parts
}

func Parse(path, source string) (Session, error) {
	s := Session{SourceType: source, SourcePath: path}
	if source != "claude" && source != "codex" && source != "gemini" && source != "opencode" && source != "trae" && source != "pi" && source != "qoder" && source != "deepseek" && source != "qwen" && source != "copilot" {
		return s, fmt.Errorf("unsupported source %q", source)
	}
	f, err := os.Open(path)
	if err != nil {
		return s, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return s, err
	}
	switch source {
	case "gemini":
		s, err = (Gemini{}).Read(f)
	case "opencode":
		s, err = parseOpenCodeFile(path)
	case "trae":
		s, err = (Trae{}).Read(f)
	case "pi":
		s, err = (Pi{}).Read(f)
	case "qoder":
		s, err = parseQoderFile(path)
	case "deepseek":
		s, err = parseDeepSeekFile(path)
	case "qwen":
		s, err = parseQwenFile(path)
	case "copilot":
		s, err = parseCopilotFile(path)
	default:
		s, err = readOriginal(f, source, path)
	}
	s.SourcePath = path
	s.Updated = info.ModTime()
	sum := sha256.Sum256([]byte(source + "\x00" + path))
	s.ID = hex.EncodeToString(sum[:16])
	return s, err
}

// readOriginal retains the exact input, including records not understood by Ferry.
func readOriginal(r io.Reader, source, path string) (Session, error) {
	var original bytes.Buffer
	s, err := read(io.TeeReader(r, &original), source, path)
	if err != nil {
		return s, err
	}
	s.Original = original.Bytes()
	return s, nil
}

func read(rdr io.Reader, source, path string) (Session, error) {
	s := Session{SourceType: source, SourcePath: path}
	scan := bufio.NewScanner(rdr)
	scan.Buffer(make([]byte, 4096), 32*1024*1024)
	var err error
	line := 0
	recognized := false
	metaSeen := false
	var entries []codexEntry
	for scan.Scan() {
		line++
		var r record
		if err = json.Unmarshal(scan.Bytes(), &r); err != nil {
			return s, fmt.Errorf("%s:%d: %w", path, line, err)
		}
		if knownRecordType(source, r.Type) {
			recognized = true
		}
		var b body
		if source == "claude" {
			if s.NativeID == "" {
				s.NativeID = r.SessionID
			}
			if s.Workspace == "" {
				s.Workspace = r.Cwd
			}
			if r.Type == "summary" {
				// Claude writes summary records when a session resumes from an
				// earlier leaf. Like Codex compaction records, they carry
				// context a continuation needs, so they join the messages.
				var summary struct {
					Summary string `json:"summary"`
				}
				if err := json.Unmarshal(scan.Bytes(), &summary); err != nil {
					return s, fmt.Errorf("%s:%d: invalid summary: %w", path, line, err)
				}
				if summary.Summary != "" {
					s.Messages = append(s.Messages, Message{Role: "summary", Timestamp: r.Timestamp, Parts: []ContentBlock{{Type: "text", Text: summary.Summary}}})
				}
				continue
			}
			if r.Type != "user" && r.Type != "assistant" {
				continue
			}
			if err := json.Unmarshal(r.Message, &b); err != nil {
				return s, fmt.Errorf("%s:%d: invalid message: %w", path, line, err)
			}
		} else {
			if r.Type == "session_meta" {
				if !metaSeen && json.Unmarshal(r.Payload, &b) == nil {
					s.NativeID = b.ID
					if s.NativeID == "" {
						s.NativeID = b.SessionID
					}
					s.Workspace = b.Cwd
					metaSeen = true
				}
				continue
			}
			if r.Type == "event_msg" {
				if json.Unmarshal(r.Payload, &b) == nil && b.Message != "" {
					role := ""
					if b.Type == "user_message" {
						role = "user"
					}
					if b.Type == "agent_message" {
						role = "assistant"
					}
					if role != "" {
						entries = append(entries, codexEntry{line: line, kind: codexEvent, message: Message{Role: role, Timestamp: r.Timestamp, Parts: []ContentBlock{{Type: "text", Text: b.Message}}}})
					}
				}
				continue
			}
			if r.Type == "compacted" {
				if json.Unmarshal(r.Payload, &b) == nil && b.Message != "" {
					entries = append(entries, codexEntry{line: line, kind: codexCompacted, message: Message{Role: "summary", Timestamp: r.Timestamp, Parts: []ContentBlock{{Type: "text", Text: b.Message}}}})
				}
				continue
			}
			if r.Type != "response_item" {
				continue
			}
			if err := json.Unmarshal(r.Payload, &b); err != nil {
				return s, fmt.Errorf("%s:%d: invalid payload: %w", path, line, err)
			}
		}
		m := Message{Role: b.Role, Timestamp: r.Timestamp}
		if source == "codex" {
			switch b.Type {
			case "message":
				if b.Role != "user" && b.Role != "assistant" {
					continue
				}
				m.Parts = contentParts(b.Content)
			case "function_call", "custom_tool_call":
				m.Role = "assistant"
				call := &ToolCall{ID: b.CallID, Name: b.Name, Namespace: b.Namespace, Input: b.Arguments, Status: acpToolCallStatus(b.Status)}
				if len(b.Input) > 0 {
					call.Input = b.Input
				}
				m.Parts = []ContentBlock{{Type: "tool_call", ToolCall: call, Raw: r.Payload}}
			case "function_call_output", "custom_tool_call_output":
				if b.CallID == "" {
					// Newer Codex delegation outputs carry no call_id; they
					// cannot pair with any call and are reported instead.
					continue
				}
				m.Role = "tool"
				result := &ToolResult{ID: b.CallID, Content: text(b.Output), IsError: b.IsError, RawContent: b.Output}
				m.Parts = []ContentBlock{{Type: "tool_result", ToolResult: result, Raw: r.Payload}}
			default:
				continue
			}
		} else {
			// Claude's outer record type is authoritative. The nested role is
			// redundant and is absent in some valid exports. Ferry's summary
			// extension is the one intentional exception: it uses an assistant
			// envelope because Claude has no summary envelope of its own.
			m.Role = r.Type
			if b.Role == "summary" {
				m.Role = "summary"
			}
			m.Parts = contentParts(b.Content)
		}
		if len(m.Parts) > 0 {
			if source == "codex" {
				entries = append(entries, codexEntry{line: line, kind: codexResponse, dialogue: b.Type == "message", message: m})
			} else {
				s.Messages = append(s.Messages, m)
			}
		}
	}
	if err = scan.Err(); err != nil {
		return s, err
	}
	if source == "codex" {
		s.Messages = append(s.Messages, codexMessages(entries)...)
	}
	if !recognized {
		return s, fmt.Errorf("%s: no recognizable %s records", path, source)
	}
	if s.NativeID == "" && path != "" {
		s.NativeID = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	return s, nil
}

// knownRecordType is deliberately narrower than “valid JSON object”. A
// reader must not silently accept another provider's JSONL as an empty
// session merely because every line parses. Known records with no dialogue
// are still accepted so their exact bytes can be restored.
func knownRecordType(source, recordType string) bool {
	switch source {
	case "claude":
		switch recordType {
		case "user", "assistant", "system", "progress", "summary", "file-history-snapshot", "queue-operation", "custom-title":
			return true
		}
	case "codex":
		switch recordType {
		case "session_meta", "turn_context", "response_item", "event_msg", "compacted":
			return true
		}
	}
	return false
}

type codexEntryKind uint8

const (
	codexEvent codexEntryKind = iota
	codexCompacted
	codexResponse
)

type codexEntry struct {
	line     int
	kind     codexEntryKind
	dialogue bool
	message  Message
}

// codexMessages keeps records in file order. Codex event_msg records are a
// display stream that is often followed immediately by the equivalent
// response_item message. Only that adjacent shape is treated as a duplicate.
// Ambiguous repeated text in separate turns is retained. A contiguous event
// run immediately before a matching response is compared entry by entry, so
// an unrelated event in that run is never discarded as collateral.
func codexMessages(entries []codexEntry) []Message {
	drop := make([]bool, len(entries))
	for i := 0; i < len(entries); {
		if entries[i].kind != codexEvent {
			i++
			continue
		}
		start := i
		for i < len(entries) && entries[i].kind == codexEvent {
			i++
		}
		end := i
		if end < len(entries) && entries[end].kind == codexResponse && entries[end].dialogue && codexEventMatches(entries[start:end], entries[end].message) {
			for j := start; j < end; j++ {
				if codexTextMessageEqual(entries[j].message, entries[end].message) {
					drop[j] = true
				}
			}
		}
		if start > 0 && entries[start-1].kind == codexResponse && entries[start-1].dialogue {
			for j := start; j < end; j++ {
				if codexTextMessageEqual(entries[j].message, entries[start-1].message) {
					drop[j] = true
				}
			}
		}
	}
	messages := make([]Message, 0, len(entries))
	for i, entry := range entries {
		if !drop[i] {
			messages = append(messages, entry.message)
		}
	}
	return messages
}

func codexEventMatches(events []codexEntry, response Message) bool {
	for _, event := range events {
		if codexTextMessageEqual(event.message, response) {
			return true
		}
	}
	return false
}

func codexTextMessageEqual(a, b Message) bool {
	return a.Role == b.Role && len(a.Parts) == 1 && len(b.Parts) == 1 && a.Parts[0].Type == "text" && b.Parts[0].Type == "text" && a.Parts[0].Text == b.Parts[0].Text
}
