// These tests pin the second review round: qualified OpenCode tool names,
// Gemini results split from assistant messages, Gemini as a conversion
// target, duplicate call rejection, Claude thinking reporting, and Codex
// delegation outputs.
package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Gemini metadata results must split into tool-role messages so every
// writer can represent them.
func TestGeminiResultsSplitForEveryTarget(t *testing.T) {
	source := []byte(`{"sessionId":"123e4567-e89b-12d3-a456-426614174040","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:02Z","messages":[{"id":"u1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"read it"},{"id":"a1","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"opening"}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"path":"x"},"status":"success","timestamp":"2026-01-01T00:00:01Z","result":[{"text":"ok"}]}]}]}`)
	s, err := (Gemini{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || s.Messages[2].Role != "tool" || s.Messages[2].Parts[0].ToolResult == nil {
		t.Fatalf("result not split: %+v", s.Messages)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	readBack, err := convertTarget(t, transfer)
	if err != nil {
		t.Fatal(err)
	}
	var results int
	for _, m := range readBack.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" {
				results++
			}
		}
	}
	if results != 1 {
		t.Fatalf("result lost in claude target")
	}
	// Codex and OpenCode need an absolute workspace; a Gemini file only has
	// the project hash, so those targets reject instead of inventing one.
	if _, err := Convert(s, "codex"); err == nil || !strings.Contains(err.Error(), "workspace") {
		t.Fatalf("gemini source must not invent a workspace for codex: %v", err)
	}
}

// Codex marks calls "completed"; Gemini accepts it as success. A paired
// result also derives the status for sources that carry none.
func TestGeminiTargetAcceptsForeignToolCalls(t *testing.T) {
	codexSource := []byte(reviewCodexMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"function_call","status":"completed","call_id":"c1","name":"read","arguments":"{\"path\":\"x\"}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}` + "\n")
	codexSession, err := (Codex{}).Read(bytes.NewReader(codexSource))
	if err != nil {
		t.Fatal(err)
	}
	codexSession.Workspace = "/example"
	transfer, err := Convert(codexSession, "gemini")
	if err != nil {
		t.Fatalf("codex->gemini: %v", err)
	}
	if !strings.Contains(string(transfer.Target), `"status":"success"`) {
		t.Fatalf("completed status not mapped:\n%s", transfer.Target)
	}
	readBack, err := convertTarget(t, transfer)
	if err != nil {
		t.Fatal(err)
	}
	if got := readBack.Messages[0].Parts[0].ToolCall.Status; got != "completed" {
		t.Fatalf("Gemini target did not preserve completed status: %q", got)
	}
	if strings.Contains(strings.Join(transfer.Limitations, "\n"), "status \"completed\" was not preserved") {
		t.Fatalf("status preservation was incorrectly reported as loss: %v", transfer.Limitations)
	}

	claudeSource := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174041","cwd":"/example","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"run"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174041","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"x"}}]}}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174041","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"done"}]}}
`)
	claudeSession, err := (Claude{}).Read(bytes.NewReader(claudeSource))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(claudeSession, "gemini"); err != nil {
		t.Fatalf("claude->gemini: %v", err)
	}

	opencodeSession := readOpenCodeFixture(t)
	if _, err := Convert(opencodeSession, "gemini"); err != nil {
		t.Fatalf("opencode->gemini: %v", err)
	}
}

// OpenCode tool names carry the qualified identity so MCP calls stay
// attributable; duplicate call ids are rejected instead of collapsed.
func TestOpenCodeQualifiedNamesAndDuplicateCalls(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"function_call","call_id":"c1","namespace":"mcp__srv","name":"js","arguments":"{}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transfer.Target), `"tool": "mcp__srv__js"`) {
		t.Fatalf("qualified tool name missing:\n%s", transfer.Target)
	}

	duplicate := Session{NativeID: "ses_dup00000000000000000000", Workspace: "/example", Messages: []Message{
		{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "x"}}},
		{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
			{Type: "tool_call", ToolCall: &ToolCall{ID: "dup", Name: "a", Input: json.RawMessage(`{}`)}},
			{Type: "tool_call", ToolCall: &ToolCall{ID: "dup", Name: "b", Input: json.RawMessage(`{}`)}},
		}},
	}}
	var out bytes.Buffer
	if err := (OpenCode{}).Write(&out, duplicate); err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("duplicate call ids not rejected: %v", err)
	}
}

// Claude thinking blocks are skipped from the projection and reported.
func TestClaudeThinkingReported(t *testing.T) {
	source := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174042","cwd":"/example","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174042","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning here","signature":"sig"},{"type":"text","text":"answer"}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Messages {
		for _, p := range m.Parts {
			if p.Type == "thinking" || p.Type == "unknown" {
				t.Fatalf("thinking block entered the model: %+v", p)
			}
		}
	}
	transfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "1 thinking blocks") {
		t.Fatalf("thinking not reported: %v", transfer.Limitations)
	}
}

// Codex delegation outputs without call_id cannot pair; they are reported.
func TestCodexDelegationOutputReported(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"function_call_output","id":"fco_1","name":"create_thread","namespace":"codex_app","output":"delegated"}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "<delegation output without call_id>=1") {
		t.Fatalf("delegation output not reported: %v", transfer.Limitations)
	}
}

// Storage sessions reject misplaced files, duplicate ids, foreign messages,
// and archives that attach parts to the wrong message.
func TestOpenCodeStorageGuards(t *testing.T) {
	root := t.TempDir()
	sessionID := "ses_guard0000000000000000000"
	wrong := filepath.Join(root, "elsewhere", sessionID+".json")
	if err := os.MkdirAll(filepath.Dir(wrong), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrong, []byte(`{"id":"`+sessionID+`","directory":"/x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(wrong, "opencode"); err == nil || !strings.Contains(err.Error(), "not inside an OpenCode storage tree") {
		t.Fatalf("misplaced session accepted: %v", err)
	}

	sessionFile := writeOpenCodeStorage(t, root)
	s, err := Parse(sessionFile, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	// Reorder the archive so the part follows another message.
	lines := bytes.Split(bytes.TrimSpace(s.Original), []byte("\n"))
	if len(lines) != 4 {
		t.Fatalf("unexpected archive shape: %d lines", len(lines))
	}
	reordered := append([]byte{}, lines[0]...)
	reordered = append(reordered, '\n')
	reordered = append(reordered, lines[2]...) // assistant message
	reordered = append(reordered, '\n')
	reordered = append(reordered, lines[1]...) // user message before its part
	reordered = append(reordered, '\n')
	reordered = append(reordered, lines[3]...) // part of the assistant message
	reordered = append(reordered, '\n')
	if _, err := (OpenCode{}).Read(bytes.NewReader(reordered)); err == nil || !strings.Contains(err.Error(), "does not belong to message") {
		t.Fatalf("misattributed part accepted: %v", err)
	}
}
