// These tests cover the review round that found false limitation reports,
// silent reasoning leaks, and providers rejecting representable content.
package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// B1: Claude top-level summary records must join the messages instead of
// being reported as represented while silently dropped.
func TestClaudeSummaryRecordJoinsMessages(t *testing.T) {
	original := []byte(`{"type":"summary","summary":"Earlier work on the parser","leafUuid":"550e8400-e29b-41d4-a716-446655440000"}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"continue"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"continuing"}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || s.Messages[0].Role != "summary" || s.Messages[0].Parts[0].Text != "Earlier work on the parser" {
		t.Fatalf("summary record was not parsed: %+v", s.Messages)
	}
	transfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transfer.Target), `"message":"Earlier work on the parser"`) {
		t.Fatalf("codex target lost the summary: %s", transfer.Target)
	}
	target, err := (Codex{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Messages) != 3 || target.Messages[0].Role != "summary" {
		t.Fatalf("summary did not survive the codex round trip: %+v", target.Messages)
	}
	for _, limitation := range skippedClaudeLimitations(s, "gemini") {
		if strings.Contains(limitation, "summary") {
			t.Fatalf("parsed summary reported as unrepresented: %s", limitation)
		}
	}
}

// Trae tool outputs carrying reasoning items must not leak them through the
// structured RawContent that target writers serialize as JSON text.
func TestTraeToolOutputReasoningIsStrippedFromProjection(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"function_call","call_id":"c1","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"reasoning_text","text":"SECRET-THOUGHT"},{"type":"text","text":"public output"}]}]}}
`)
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"claude", "codex", "opencode", "pi"} {
		transfer, err := Convert(s, target)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if strings.Contains(string(transfer.Target), "SECRET-THOUGHT") {
			t.Fatalf("%s target leaked reasoning from the tool output", target)
		}
	}
}

// Trae text blocks marked source:"reasoning" are dropped; the report must say so.
func TestTraeSourceReasoningTextIsReported(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"a1","role":"assistant","content":[{"type":"text","source":"reasoning","text":"SECRET-THOUGHT"},{"type":"text","text":"visible answer"}]}]}}
`)
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transfer.Target), "SECRET-THOUGHT") {
		t.Fatal("target leaked source-reasoning text")
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "text(source=reasoning)") {
		t.Fatalf("dropped reasoning text was not reported: %v", transfer.Limitations)
	}
}

// Codex thinking blocks inside message content must be counted in the report.
func TestCodexThinkingBlocksAreReported(t *testing.T) {
	original := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"thinking","thinking":"SECRET-THOUGHT"},{"type":"output_text","text":"visible"}]}}
`)
	s, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transfer.Target), "SECRET-THOUGHT") {
		t.Fatal("target leaked codex thinking text")
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "Codex thinking blocks") {
		t.Fatalf("thinking block was not reported: %v", transfer.Limitations)
	}
}

// A result flag must win over the call status in the Gemini projection, and
// the resulting status change must be reported.
func TestGeminiResultFlagWinsOverCallStatus(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"function_call","call_id":"c9","status":"completed","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"c9","output":"boom","is_error":true}]}}
`)
	s, err := (Trae{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Gemini{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.IsError {
				failed = true
			}
		}
	}
	if !failed {
		t.Fatalf("failed tool became a success in the gemini projection: %+v", target.Messages)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, `status changed from "completed"`) {
		t.Fatalf("status conflict was not reported: %v", transfer.Limitations)
	}
}

// Codex output no longer invents an error flag, so the drop is reported; the
// reader still honors the flag in older development snapshots.
func TestCodexErrorFlagIsReportedNotInvented(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"failed","is_error":true}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(transfer.Target), `"is_error"`) {
		t.Fatal("codex target invented an error flag its format does not carry")
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "flag is dropped") {
		t.Fatalf("error flag loss was not reported: %v", transfer.Limitations)
	}

	legacy := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call","call_id":"call-1","name":"run","arguments":"{}"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"function_call_output","call_id":"call-1","output":"failed","is_error":true}}
`)
	codex, err := (Codex{}).Read(bytes.NewReader(legacy))
	if err != nil {
		t.Fatal(err)
	}
	if !codex.Messages[1].Parts[0].ToolResult.IsError {
		t.Fatal("codex reader ignored an explicit error flag")
	}
	back, err := Convert(codex, "claude")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Claude{}).Read(bytes.NewReader(back.Target))
	if err != nil {
		t.Fatal(err)
	}
	preserved := false
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.IsError {
				preserved = true
			}
		}
	}
	if !preserved {
		t.Fatal("legacy codex error flag was lost on the way to claude")
	}
}

// Claude stores tool results in user records; the Pi writer must accept that
// shape instead of rejecting most real Claude sessions.
func TestPiAcceptsClaudeToolResultUserRecord(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"run the check"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"check","input":{"path":"x"}}]}}
{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"failed","is_error":true}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "pi")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Pi{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil && part.ToolResult.ID == "call-1" && part.ToolResult.IsError {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("pi target lost the claude tool result: %+v", target.Messages)
	}
}

// Trae keeps free-text tool input unwrapped like Codex, so the wrap claim
// must not be made for it.
func TestFreeTextInputLimitationIsAccurateForTrae(t *testing.T) {
	original := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"custom_tool_call","call_id":"call-1","name":"patch","input":"apply to file x"}}
`)
	s, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "trae")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if strings.Contains(joined, "wrapped in an object argument for trae") {
		t.Fatalf("trae was described as wrapping free-text input: %v", transfer.Limitations)
	}
	if !strings.Contains(string(transfer.Target), `"input":"apply to file x"`) {
		t.Fatalf("trae target rewrote the free-text input: %s", transfer.Target)
	}
}

// OpenCode stores millisecond timestamps; sub-millisecond precision is
// truncated and reported instead of failing the conversion.
func TestOpenCodeSubMillisecondTimestampIsTruncatedAndReported(t *testing.T) {
	original := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01.000500000Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}
`)
	s, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "opencode")
	if err != nil {
		t.Fatalf("sub-millisecond timestamp failed the conversion: %v", err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "millisecond precision") {
		t.Fatalf("timestamp truncation was not reported: %v", transfer.Limitations)
	}
}

// Trae drops user records that consist only of injected context envelopes and
// re-creates that context natively, so the writer skips them and reports.
func TestTraeEnvelopeContextIsSkippedAndReported(t *testing.T) {
	original := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<INSTRUCTIONS>AGENTS.md contents</INSTRUCTIONS>"}]}}
{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"real question"}]}}
{"type":"response_item","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}
`)
	s, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "trae")
	if err != nil {
		t.Fatalf("envelope context made the conversion fail: %v", err)
	}
	if strings.Contains(string(transfer.Target), "AGENTS.md contents") {
		t.Fatal("envelope context was written although Trae drops it on read-back")
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "injected context envelope text part(s)") {
		t.Fatalf("envelope removal was not reported: %v", transfer.Limitations)
	}
	target, err := (Trae{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	if len(target.Messages) != 2 || target.Messages[0].Parts[0].Text != "real question" {
		t.Fatalf("real dialogue was lost: %+v", target.Messages)
	}
}

// Trae collapses identical tool-call replays on read-back, so the writer must
// refuse them up front with a clear reason.
func TestTraeRejectsIdenticalToolCallReplays(t *testing.T) {
	call := func() ContentBlock {
		return ContentBlock{Type: "tool_call", ToolCall: &ToolCall{ID: "dup", Name: "run", Input: json.RawMessage(`{}`)}}
	}
	s := Session{NativeID: "123e4567-e89b-12d3-a456-426614174000", Workspace: "/work", Messages: []Message{{
		Role:      "assistant",
		Timestamp: "2026-01-01T00:00:00Z",
		Parts:     []ContentBlock{call(), call()},
	}}}
	err := (Trae{}).Write(&bytes.Buffer{}, s)
	if err == nil || !strings.Contains(err.Error(), "identical replays") {
		t.Fatalf("expected a clear duplicate-replay error, got %v", err)
	}
}

// Empty Pi messages vanish from the projection; the report must say so.
func TestPiEmptyMessagesAreReported(t *testing.T) {
	original := []byte(`{"type":"session","version":3,"id":"pid","timestamp":"2026-01-01T00:00:00Z"}
{"type":"message","id":"e1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[]}}
{"type":"message","id":"e2","parentId":"e1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"visible"}}
`)
	s, err := (Pi{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "message.content=<empty>") {
		t.Fatalf("empty pi message was not reported: %v", transfer.Limitations)
	}
}

// Pi carries block-shaped structured results natively; only shapes it cannot
// represent may be reported as flattened to JSON text.
func TestPiStructuredResultLimitationIsAccurate(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"function_call","call_id":"c1","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"text","text":"native blocks"}]},{"type":"function_call","call_id":"c2","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"c2","output":{"odd":"shape"}}]}}
`)
	s, err := (Trae{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "pi")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "1 structured tool results serialized as JSON text") {
		t.Fatalf("expected exactly the unrepresentable shape to be reported: %v", transfer.Limitations)
	}
	if !strings.Contains(string(transfer.Target), "native blocks") {
		t.Fatal("pi target lost the native block content")
	}
}

// Trae collapses identical tool-result replays too, so the writer refuses
// them up front with the same clear reason as duplicate calls.
func TestTraeRejectsIdenticalToolResultReplays(t *testing.T) {
	result := func() ContentBlock {
		return ContentBlock{Type: "tool_result", ToolResult: &ToolResult{ID: "dup", Content: "same"}}
	}
	s := Session{NativeID: "123e4567-e89b-12d3-a456-426614174000", Workspace: "/work", Messages: []Message{{
		Role:      "tool",
		Timestamp: "2026-01-01T00:00:00Z",
		Parts:     []ContentBlock{result(), result()},
	}}}
	err := (Trae{}).Write(&bytes.Buffer{}, s)
	if err == nil || !strings.Contains(err.Error(), `tool result "dup"`) {
		t.Fatalf("expected a clear duplicate-result error, got %v", err)
	}
}

// Reasoning stripped from a Trae tool output must be reported like
// message-content reasoning.
func TestTraeToolOutputReasoningIsReported(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"function_call","call_id":"c1","name":"run","arguments":"{}"},{"type":"function_call_output","call_id":"c1","output":[{"type":"reasoning_text","text":"SECRET-THOUGHT"},{"type":"text","text":"public output"}]}]}}
`)
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "message.content=reasoning_text") {
		t.Fatalf("stripped tool-output reasoning was not reported: %v", transfer.Limitations)
	}
}
