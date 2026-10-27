// These tests pin the fixes from the review round: content that cannot cross
// providers must be reported, never dropped or altered silently.
package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reviewCodexMeta = `{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/example"}}` + "\n"

// P0-1: developer and system role messages carry real instructions; they
// must be reported as unrepresented instead of vanishing.
func TestConvertReportsCodexRoleMessages(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"app context"}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Messages {
		if m.Role == "developer" || m.Role == "system" {
			t.Fatalf("role message entered the model: %+v", m)
		}
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "developer=1") {
		t.Fatalf("developer message not reported: %v", transfer.Limitations)
	}
}

// P0-2: compacted records can carry the replacement history with an empty
// summary; the unexpanded history must be reported.
func TestConvertReportsCompactedReplacementHistory(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"user","content":[{"type":"input_text","text":"old"}]}]}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "1 compacted records whose replacement histories are not expanded") {
		t.Fatalf("replacement history not reported: %v", transfer.Limitations)
	}
}

// P1-3: the Codex namespace qualifies MCP tool calls and must survive
// round trips; Claude receives the qualified single name.
func TestCodexNamespaceRoundTrip(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"function_call","call_id":"c1","namespace":"mcp__srv","name":"js","arguments":"{}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"function_call_output","call_id":"c1","output":"ok"}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if call := s.Messages[0].Parts[0].ToolCall; call.Namespace != "mcp__srv" || call.Name != "js" {
		t.Fatalf("namespace not read: %+v", call)
	}
	var back bytes.Buffer
	if err := (Codex{}).Write(&back, s); err != nil {
		t.Fatal(err)
	}
	again, err := (Codex{}).Read(&back)
	if err != nil {
		t.Fatal(err)
	}
	if call := again.Messages[0].Parts[0].ToolCall; call.Namespace != "mcp__srv" {
		t.Fatalf("namespace not written back: %+v", call)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, m := range target.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_call" {
				names = append(names, p.ToolCall.Name)
			}
		}
	}
	if len(names) != 1 || names[0] != "mcp__srv__js" {
		t.Fatalf("qualified name not written to Claude: %v", names)
	}
}

// P1-4: the Claude error flag survives Claude and OpenCode round trips and
// is reported when the target has no flag.
func TestToolResultErrorFlag(t *testing.T) {
	source := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174010","cwd":"/example","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"run it"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174010","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"Bash","input":{"command":"x"}}]}}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174010","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"boom","is_error":true}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	var flagged int
	for _, m := range s.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.ToolResult.IsError {
				flagged++
			}
		}
	}
	if flagged != 1 {
		t.Fatalf("is_error not read: %d", flagged)
	}
	var claudeBack bytes.Buffer
	if err := (Claude{}).Write(&claudeBack, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(claudeBack.String(), `"is_error":true`) {
		t.Fatal("is_error not written back to Claude")
	}
	transfer, err := Convert(s, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (OpenCode{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	var errorStates int
	for _, m := range target.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.ToolResult.IsError {
				errorStates++
			}
		}
	}
	if errorStates != 1 {
		t.Fatalf("OpenCode error status not preserved: %d", errorStates)
	}
	codexTransfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(codexTransfer.Limitations, "\n"), "flagged as errors") {
		t.Fatalf("dropped error flag not reported: %v", codexTransfer.Limitations)
	}
	geminiTransfer, err := Convert(s, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(geminiTransfer.Limitations, "\n"), "has no error flag") {
		t.Fatalf("Gemini error support was incorrectly reported as loss: %v", geminiTransfer.Limitations)
	}
	geminiTarget, err := (Gemini{}).Read(bytes.NewReader(geminiTransfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	var geminiErrors int
	for _, m := range geminiTarget.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_result" && p.ToolResult != nil && p.ToolResult.IsError {
				geminiErrors++
			}
		}
	}
	if geminiErrors != 1 {
		t.Fatalf("Gemini did not reconstruct the failed tool result: %d", geminiErrors)
	}
}

// P1-5: integers above float64 precision must survive Gemini writes and stay
// distinguishable in projection normalization.
func TestExactIntegersSurviveGeminiWrite(t *testing.T) {
	left, err := canonicalToolInput(json.RawMessage(`{"n":9007199254740993}`))
	if err != nil {
		t.Fatal(err)
	}
	right, err := canonicalToolInput(json.RawMessage(`{"n":9007199254740992}`))
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("canonicalization calls two different large integers equal")
	}
	source := []byte(`{"sessionId":"123e4567-e89b-12d3-a456-426614174020","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:01Z","messages":[{"id":"a1","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"checking"}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"n":9007199254740993},"status":"success","timestamp":"2026-01-01T00:00:01Z"}]}]}`)
	s, err := (Gemini{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	s.Workspace = "/example"
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "9007199254740993") {
		t.Fatalf("large integer corrupted in Gemini output:\n%s", out.String())
	}
	back, err := (Gemini{}).Read(&out)
	if err != nil {
		t.Fatal(err)
	}
	var input string
	for _, m := range back.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_call" {
				input = string(p.ToolCall.Input)
			}
		}
	}
	if !strings.Contains(input, "9007199254740993") {
		t.Fatalf("large integer corrupted on read-back: %s", input)
	}
}

// P2-6: a null $set.messages must not clear the session; the official
// history service only replaces actual arrays.
func TestGeminiSetMessagesNullKeepsMessages(t *testing.T) {
	source := []byte(
		`{"sessionId":"gem-null","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:01Z"}` + "\n" +
			`{"id":"u1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"hello"}` + "\n" +
			`{"$set":{"messages":null}}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("null $set.messages cleared the session: %+v", s.Messages)
	}
}

// P3-7: a timestamp the source carried must not silently change.
func TestProjectionRejectsChangedTimestamp(t *testing.T) {
	source := Session{Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hi"}}}}}
	target := Session{Messages: []Message{{Role: "user", Timestamp: "2026-01-02T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hi"}}}}}
	if err := validateProjection(source, target); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("changed timestamp not detected: %v", err)
	}
}

// P3-9: Gemini model thoughts are counted and reported, not folded into a
// generic metadata note.
func TestGeminiThoughtsReported(t *testing.T) {
	source := []byte(`{"sessionId":"gem-thoughts","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:01Z","messages":[{"id":"u1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"hello"},{"id":"a1","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"answer"}],"thoughts":[{"subject":"plan","description":"thinking"}]}]}`)
	s, err := (Gemini{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "1 messages with model thoughts") {
		t.Fatalf("thoughts not reported: %v", transfer.Limitations)
	}
}

// An OpenCode tool part couples a call and its result, so content between
// them cannot keep its order and must be rejected rather than reordered.
func TestOpenCodeRejectsSeparatedToolResult(t *testing.T) {
	s := Session{
		NativeID: "ses_sep00000000000000000000", Workspace: "/example",
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "run"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "c1", Name: "bash", Input: json.RawMessage(`{}`)}},
				{Type: "text", Text: "noted"},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "c1", Content: "ok"}},
			}},
		},
	}
	var out bytes.Buffer
	if err := (OpenCode{}).Write(&out, s); err == nil || !strings.Contains(err.Error(), "separated from its call") {
		t.Fatalf("expected separation rejection, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("rejection must not emit output")
	}
}

// Codex custom tool calls carry free-text input and structured output; both
// must cross providers without silent loss.
func TestCustomToolCallCrossProvider(t *testing.T) {
	source := []byte(reviewCodexMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"custom_tool_call","call_id":"c1","name":"exec","input":"console.log('hi')"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"c1","output":{"stdout":"hi\n","exit":0}}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	var back bytes.Buffer
	if err := (Codex{}).Write(&back, s); err != nil {
		t.Fatal(err)
	}
	again, err := (Codex{}).Read(&back)
	if err != nil {
		t.Fatal(err)
	}
	call := again.Messages[0].Parts[0].ToolCall
	var text string
	if err := json.Unmarshal(call.Input, &text); err != nil || text != "console.log('hi')" {
		t.Fatalf("free-text input not preserved: %s", call.Input)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "1 custom tool calls with free-text input") {
		t.Fatalf("free-text wrapping not reported: %v", transfer.Limitations)
	}
	if !strings.Contains(joined, "1 structured tool results serialized as JSON text") {
		t.Fatalf("structured output not reported: %v", transfer.Limitations)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	var wrappedInput bool
	var serialized string
	for _, m := range target.Messages {
		for _, p := range m.Parts {
			if p.Type == "tool_call" && strings.Contains(string(p.ToolCall.Input), "console.log('hi')") {
				wrappedInput = true
			}
			if p.Type == "tool_result" {
				serialized = p.ToolResult.Content
			}
		}
	}
	if !wrappedInput {
		t.Fatal("free-text input missing from Claude target")
	}
	if !strings.Contains(serialized, `"exit":0`) {
		t.Fatalf("structured output not serialized exactly: %q", serialized)
	}
}

// Parallel tool calls (two calls, then two results) must convert to every
// provider that couples or splits calls and results.
func TestParallelToolCallsConvert(t *testing.T) {
	source := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174030","cwd":"/example","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"run both"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174030","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"a","name":"Read","input":{"p":"1"}},{"type":"tool_use","id":"b","name":"Read","input":{"p":"2"}}]}}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174030","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"a","content":"one"},{"type":"tool_result","tool_use_id":"b","content":"two"}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"codex", "opencode"} {
		t.Run(target, func(t *testing.T) {
			transfer, err := Convert(s, target)
			if err != nil {
				t.Fatal(err)
			}
			var results int
			readBack, err := convertTarget(t, transfer)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range readBack.Messages {
				for _, p := range m.Parts {
					if p.Type == "tool_result" {
						results++
					}
				}
			}
			if results != 2 {
				t.Fatalf("parallel results lost in %s: %d", target, results)
			}
		})
	}
}

func convertTarget(t *testing.T, transfer Transfer) (Session, error) {
	t.Helper()
	switch transfer.TargetType {
	case "claude":
		return (Claude{}).Read(bytes.NewReader(transfer.Target))
	case "codex":
		return (Codex{}).Read(bytes.NewReader(transfer.Target))
	case "gemini":
		return (Gemini{}).Read(bytes.NewReader(transfer.Target))
	case "opencode":
		return (OpenCode{}).Read(bytes.NewReader(transfer.Target))
	}
	return Session{}, fmt.Errorf("unknown target %q", transfer.TargetType)
}

// Files that are not valid JSON (for example with stray non-UTF-8 bytes)
// must still be archived byte-for-byte when they belong to a session.
func TestOpenCodeArchiveKeepsInvalidBytes(t *testing.T) {
	root := t.TempDir()
	sessionID := "ses_bytes00000000000000000000"
	projectDir := filepath.Join(root, "storage", "session", "proj")
	messageDir := filepath.Join(root, "storage", "message", sessionID)
	partDir := filepath.Join(root, "storage", "part", "msg_bytes0000000000000000b")
	for _, dir := range []string{projectDir, messageDir, partDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string][]byte{
		filepath.Join(projectDir, sessionID+".json"):                 []byte(`{"id":"` + sessionID + `","directory":"/example","time":{"created":1770000000000,"updated":1770000006000}}`),
		filepath.Join(messageDir, "msg_bytes0000000000000000b.json"): []byte(`{"id":"msg_bytes0000000000000000b","sessionID":"` + sessionID + `","role":"assistant","time":{"created":1770000001000}}`),
		filepath.Join(partDir, "prt_bytes0000000000000001.json"):     append([]byte(`{"id":"prt_bytes0000000000000001","type":"text","text":"ok`), 0xff, '}'),
	}
	for path, content := range files {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Parse(filepath.Join(projectDir, sessionID+".json"), "opencode")
	if err != nil {
		t.Fatal(err)
	}
	contents, err := openCodeReadArchive(s.Original)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, msg := range contents.Messages {
		for _, raw := range msg.Parts {
			if bytes.IndexByte(raw, 0xff) >= 0 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("invalid byte lost from the archive")
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil || !bytes.Equal(restored.Bytes(), s.Original) {
		t.Fatal("archive restore changed bytes")
	}
}

// Discovery only treats ses_*.json files as OpenCode sessions.
func TestDiscoverOpenCodeIgnoresStrayFiles(t *testing.T) {
	home := t.TempDir()
	sessionFile := writeOpenCodeStorage(t, filepath.Join(home, ".local", "share", "opencode"))
	stray := filepath.Join(filepath.Dir(sessionFile), "notes.json")
	if err := os.WriteFile(stray, []byte(`{"not":"a session"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 1 {
		t.Fatalf("got %d sessions, %d errors", len(sessions), len(errs))
	}
}
