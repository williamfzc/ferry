package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestConvertKeepsSourceAndSupportedContent(t *testing.T) {
	original := []byte("{" +
		`"type":"session_meta","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}` + "}\r\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:00Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"run it"}]}}` + "\r\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call","call_id":"call-1","name":"shell","arguments":"{\"cmd\":\"go test\",\"quiet\":true}"}}` + "\r\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"function_call_output","call_id":"call-1","output":"passed"}}` + "\r\n" +
		`{"type":"future_record","payload":{"kept":true}}` + "\r\n")

	source, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transfer.Original, original) {
		t.Fatal("conversion changed the captured source")
	}
	if transfer.SourceType != "codex" || transfer.TargetType != "claude" || len(transfer.Limitations) == 0 {
		t.Fatalf("unexpected transfer metadata: %+v", transfer)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(limitations, "future_record=1") || !strings.Contains(limitations, "session_meta=1") {
		t.Fatalf("omitted source records were not reported: %+v", transfer.Limitations)
	}

	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatalf("target is not readable: %v", err)
	}
	want, err := projectedParts(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := projectedParts(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != len(got) {
		t.Fatalf("supported content count changed: want %d, got %d", len(want), len(got))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("supported part %d changed: want %+v, got %+v", i, want[i], got[i])
		}
	}
}

func TestConvertReportsDroppedToolCallStatus(t *testing.T) {
	original := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call","status":"completed","call_id":"call-1","name":"read","arguments":"{}"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"function_call_output","call_id":"call-1","output":"ok"}}
`)
	source, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, `Tool call status "completed" on 1 call(s) was not preserved by claude`) {
		t.Fatalf("status loss was not reported: %v", transfer.Limitations)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	for _, message := range target.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_call" && part.ToolCall != nil && part.ToolCall.Status != "" {
				t.Fatalf("Claude unexpectedly recovered Codex status %q", part.ToolCall.Status)
			}
		}
	}
}

func TestConvertReportsUnknownToolCallStatus(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call","status":"queued_for_retry","call_id":"call-unknown","name":"read","arguments":"{}"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"function_call","status":"queued_for_retry","call_id":"call-unknown-2","name":"read","arguments":"{}"}}
`)
	source, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), `Tool call status "queued_for_retry" on 2 call(s) has no ACP equivalent`) {
		t.Fatalf("unknown status loss was not reported: %v", transfer.Limitations)
	}
}

func TestPairToolResultsPreservesEachParallelResultOnce(t *testing.T) {
	parts := []projectedPart{
		{Type: "text", Text: "run both"},
		{Type: "tool_call", ID: "a", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_call", ID: "b", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_result", ID: "b", Text: "two", Timestamp: "result-time"},
		{Type: "tool_result", ID: "a", Text: "one", Timestamp: "result-time"},
	}
	want := []projectedPart{
		{Type: "text", Text: "run both"},
		{Type: "tool_call", ID: "a", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_result", ID: "a", Text: "one", Timestamp: "result-time"},
		{Type: "tool_call", ID: "b", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_result", ID: "b", Text: "two", Timestamp: "result-time"},
	}
	if got := pairToolResults(parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("parallel call/result order changed or results duplicated: got %+v want %+v", got, want)
	}
}

func TestPairToolResultsKeepsUnmatchedResultsOnce(t *testing.T) {
	parts := []projectedPart{
		{Type: "tool_result", ID: "missing", Text: "orphan"},
		{Type: "tool_call", ID: "a", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_result", ID: "a", Text: "one", Timestamp: "result-time"},
	}
	want := []projectedPart{
		{Type: "tool_call", ID: "a", Name: "Read", Timestamp: "call-time"},
		{Type: "tool_result", ID: "a", Text: "one", Timestamp: "result-time"},
		{Type: "tool_result", ID: "missing", Text: "orphan", Timestamp: ""},
	}
	if got := pairToolResults(parts); !reflect.DeepEqual(got, want) {
		t.Fatalf("unmatched result changed or duplicated: got %+v want %+v", got, want)
	}
}

func TestValidateProjectionRejectsChangedPairedResultTimestamp(t *testing.T) {
	source := Session{Messages: []Message{
		{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "tool_call", ToolCall: &ToolCall{ID: "a", Name: "Read", Input: []byte(`{}`)}}}},
		{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "a", Content: "one"}}}},
	}}
	target := source
	target.Messages = append([]Message(nil), source.Messages...)
	target.Messages[1].Timestamp = "2026-01-01T00:00:03Z"
	if err := validateProjection(source, target); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("changed paired result timestamp was accepted: %v", err)
	}
}

func TestConvertReportsReorderedToolResults(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "ses_parallel000000000000000000",
		Workspace:  "/example/project",
		Original:   []byte("source snapshot"),
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "run both"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "a", Name: "Read", Input: []byte(`{"path":"a"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "b", Name: "Read", Input: []byte(`{"path":"b"}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "b", Content: "two"}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "a", Content: "one"}},
			}},
		},
	}
	transfer, err := Convert(source, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "reordered supported parts") || !strings.Contains(joined, "tool_result:b, tool_result:a") {
		t.Fatalf("result reorder was not reported: %v", transfer.Limitations)
	}
	readBack, err := (OpenCode{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	if got := toolResultOrder(readBack); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("unexpected target result order: %v", got)
	}
}

func TestConvertReportsInterleavingReorder(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "ses_interleave0000000000000000",
		Workspace:  "/example/project",
		Original:   []byte("source snapshot"),
		Messages: []Message{
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "a", Name: "Read", Input: []byte(`{"path":"a"}`)}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "a", Content: "one"}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "b", Name: "Read", Input: []byte(`{"path":"b"}`)}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "b", Content: "two"}},
			}},
		},
	}
	transfer, err := Convert(source, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "reordered supported parts") || !strings.Contains(joined, "tool_call:a, tool_call:b, tool_result:a, tool_result:b") {
		t.Fatalf("interleaving reorder was not reported: %v", transfer.Limitations)
	}
}

func TestConvertDoesNotAliasCapturedBytes(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}` + "\n")
	source, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	first, err := Convert(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	wantOriginal, wantTarget := bytes.Clone(first.Original), bytes.Clone(first.Target)
	first.Original[0] = 'X'
	first.Target[0] = 'X'
	second, err := Convert(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source.Original, wantOriginal) {
		t.Fatal("returned original aliases source bytes")
	}
	if !bytes.Equal(second.Original, wantOriginal) || !bytes.Equal(second.Target, wantTarget) {
		t.Fatal("returned conversion buffers were not independent")
	}
}

func TestConvertSameProviderValidatesOriginalIdentity(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "session-a",
		Workspace:  "/work/a",
		Original:   []byte(`{"type":"user","sessionId":"session-b","cwd":"/work/b","message":{"role":"user","content":"hello"}}` + "\n"),
	}
	if _, err := Convert(source, "claude"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched original identity was accepted: %v", err)
	}
}

func TestValidateTargetIdentityRejectsChangedMetadata(t *testing.T) {
	source := Session{SourceType: "claude", NativeID: "id", Workspace: "/work"}
	for _, target := range []Session{
		{SourceType: "codex", NativeID: "other", Workspace: "/work"},
		{SourceType: "codex", NativeID: "id", Workspace: "/other"},
		{SourceType: "claude", NativeID: "id", Workspace: "/work"},
	} {
		if err := validateTargetIdentity(source, target, "codex"); err == nil {
			t.Fatalf("invalid target identity was accepted: %+v", target)
		}
	}
}

func TestConvertRejectsUnsupportedContent(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "550e8400-e29b-41d4-a716-446655440000",
		Original:   []byte("source snapshot"),
		Messages:   []Message{{Role: "user", Parts: []ContentBlock{{Type: "image", Raw: json.RawMessage(`{"type":"image"}`)}}}},
	}
	if _, err := Convert(source, "codex"); err == nil || !strings.Contains(err.Error(), `unsupported content "image"`) {
		t.Fatalf("unsupported content was accepted: %v", err)
	}
}

func TestConvertRejectsSessionWithoutSupportedMessages(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "550e8400-e29b-41d4-a716-446655440000",
		Original:   []byte(`{"type":"system","sessionId":"id","subtype":"init"}` + "\n"),
	}
	if _, err := Convert(source, "codex"); err == nil || !strings.Contains(err.Error(), "no supported messages") {
		t.Fatalf("empty conversion was accepted: %v", err)
	}
}

func TestConvertPreservesImageAcrossProviders(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "550e8400-e29b-41d4-a716-446655440000",
		Workspace:  "/work",
		Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{
			Type: "image",
			Raw:  json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`),
		}}}},
		Original: []byte("source"),
	}
	transfer, err := Convert(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Codex{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	want, err := projectedParts(source)
	if err != nil {
		t.Fatal(err)
	}
	got, err := projectedParts(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(want) != 1 || len(got) != 1 || want[0] != got[0] {
		t.Fatalf("image projection changed: want %+v, got %+v", want, got)
	}
}

func TestConvertReportsSkippedCodexSubtypes(t *testing.T) {
	original := []byte("{" +
		`"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work"}` + "}\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"reasoning","text":"hidden"}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"token_count","input_tokens":4}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"keep"}]}}` + "\n")
	source, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(limitations, "response_item subtypes (reasoning=1)") || !strings.Contains(limitations, "event_msg subtypes (token_count=1)") {
		t.Fatalf("skipped Codex subtypes were not reported: %+v", transfer.Limitations)
	}
}

func TestValidateProjectionDetectsDroppedContent(t *testing.T) {
	source := Session{Messages: []Message{{Role: "user", Parts: []ContentBlock{{Type: "text", Text: "keep"}}}}}
	target := Session{Messages: []Message{{Role: "user", Parts: []ContentBlock{{Type: "text", Text: "changed"}}}}}
	if err := validateProjection(source, target); err == nil {
		t.Fatal("changed supported content was accepted")
	}
}

func toolResultOrder(s Session) []string {
	order := make([]string, 0)
	for _, message := range s.Messages {
		for _, part := range message.Parts {
			if part.Type == "tool_result" && part.ToolResult != nil {
				order = append(order, part.ToolResult.ID)
			}
		}
	}
	return order
}

func TestNullToolResultIsNotCollapsedToEmptyText(t *testing.T) {
	const sourceText = `{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:00Z","payload":{"type":"function_call","call_id":"call-null","name":"read","arguments":"{}"}}
{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call_output","call_id":"call-null","output":null}}
`
	source, err := (Codex{}).Read(bytes.NewBufferString(sourceText))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Messages) != 2 {
		t.Fatalf("expected call and result messages, got %d", len(source.Messages))
	}
	result := source.Messages[1].Parts[0].ToolResult
	if result == nil || result.Content != "null" || string(result.RawContent) != "null" {
		t.Fatalf("null result was changed while reading: %+v", result)
	}

	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatalf("null result conversion failed: %v", err)
	}
	if !strings.Contains(string(transfer.Target), `"content":"null"`) {
		t.Fatalf("target did not retain null as visible JSON text: %s", transfer.Target)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "structured tool results") {
		t.Fatalf("null result was not reported as structured content: %v", transfer.Limitations)
	}
}

func TestNullToolResultTextHelpersPreserveValue(t *testing.T) {
	for name, got := range map[string]string{
		"generic": text([]byte(" null ")),
		"pi":      piToolResultText([]byte("null")),
		"trae":    traeToolResultText([]byte("null")),
	} {
		if got != "null" {
			t.Errorf("%s helper returned %q for JSON null", name, got)
		}
	}
	content := piToolResultContent(&ToolResult{RawContent: []byte("null")})
	blocks, ok := content.([]any)
	if !ok || len(blocks) != 1 || blocks[0].(map[string]any)["text"] != "null" {
		t.Fatalf("Pi writer dropped JSON null result: %#v", content)
	}
}

func TestNullToolResultCountsAsStructured(t *testing.T) {
	s := Session{Messages: []Message{{Role: "tool", Parts: []ContentBlock{{
		Type: "tool_result", ToolResult: &ToolResult{ID: "call-null", RawContent: []byte("null")},
	}}}}}
	if got := structuredResultCount(s); got != 1 {
		t.Fatalf("JSON null was not counted as structured output: %d", got)
	}
}

// The Claude error flag survives Claude and OpenCode round trips and
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

// A timestamp the source carried must not silently change.
func TestProjectionRejectsChangedTimestamp(t *testing.T) {
	source := Session{Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hi"}}}}}
	target := Session{Messages: []Message{{Role: "user", Timestamp: "2026-01-02T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hi"}}}}}
	if err := validateProjection(source, target); err == nil || !strings.Contains(err.Error(), "timestamp") {
		t.Fatalf("changed timestamp not detected: %v", err)
	}
}

// Codex custom tool calls carry free-text input and structured output; both
// must cross providers without silent loss.
func TestCustomToolCallCrossProvider(t *testing.T) {
	source := []byte(codexTestMeta +
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

func TestConvertReportsUnpairedToolCallsAndResultsWithoutContent(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "550e8400-e29b-41d4-a716-446655440000",
		Workspace:  "/work",
		Original: []byte(`{"type":"user","sessionId":"550e8400-e29b-41d4-a716-446655440000","cwd":"/work","message":{"role":"user","content":"hello"}}
`),
		Messages: []Message{
			{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "open-call", Name: "read", Input: []byte(`{"path":"TOP_SECRET_INPUT"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "duplicate", Name: "read", Input: []byte(`{"path":"duplicate-a"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "duplicate", Name: "read", Input: []byte(`{"path":"duplicate-b"}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "duplicate", Content: "TOP_SECRET_OUTPUT"}},
			}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "parallel-a", Name: "read", Input: []byte(`{"path":"a"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "parallel-b", Name: "read", Input: []byte(`{"path":"b"}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:03Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "parallel-b", Content: "b"}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "parallel-a", Content: "a"}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "orphan-result", Content: "orphan"}},
			}},
		},
	}

	transfer, err := Convert(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	for _, want := range []string{
		"2 unpaired tool calls without results",
		"duplicate=1",
		"open-call=1",
		"1 unpaired tool results without calls",
		"orphan-result=1",
	} {
		if !strings.Contains(limitations, want) {
			t.Fatalf("limitations missing %q: %v", want, transfer.Limitations)
		}
	}
	for _, secret := range []string{"TOP_SECRET_INPUT", "TOP_SECRET_OUTPUT"} {
		if strings.Contains(limitations, secret) {
			t.Fatalf("limitation exposed tool content %q: %s", secret, limitations)
		}
	}

	readBack, err := (Codex{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatalf("target is not readable: %v", err)
	}
	var openCalls, duplicateCalls int
	for _, message := range readBack.Messages {
		for _, part := range message.Parts {
			if part.Type != "tool_call" || part.ToolCall == nil {
				continue
			}
			switch part.ToolCall.ID {
			case "open-call":
				openCalls++
			case "duplicate":
				duplicateCalls++
			}
		}
	}
	if openCalls != 1 || duplicateCalls != 2 {
		t.Fatalf("open or duplicate calls were not preserved: open=%d duplicate=%d messages=%+v", openCalls, duplicateCalls, readBack.Messages)
	}
}

func TestConvertReportsBalancedDuplicateToolIDsWithoutContent(t *testing.T) {
	source := Session{
		SourceType: "claude",
		NativeID:   "550e8400-e29b-41d4-a716-446655440001",
		Workspace:  "/work",
		Original: []byte(`{"type":"user","sessionId":"550e8400-e29b-41d4-a716-446655440001","cwd":"/work","message":{"role":"user","content":"hello"}}
`),
		Messages: []Message{
			{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "balanced", Name: "read", Input: []byte(`{"path":"DUPLICATE_SECRET_A"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "balanced", Name: "read", Input: []byte(`{"path":"DUPLICATE_SECRET_B"}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "balanced", Content: "DUPLICATE_OUTPUT_A"}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "balanced", Content: "DUPLICATE_OUTPUT_B"}},
			}},
		},
	}

	transfer, err := Convert(source, "codex")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	for _, want := range []string{
		"duplicate tool call IDs (balanced=2)",
		"duplicate tool result IDs (balanced=2)",
	} {
		if !strings.Contains(limitations, want) {
			t.Fatalf("limitations missing %q: %v", want, transfer.Limitations)
		}
	}
	for _, secret := range []string{"DUPLICATE_SECRET_A", "DUPLICATE_SECRET_B", "DUPLICATE_OUTPUT_A", "DUPLICATE_OUTPUT_B"} {
		if strings.Contains(limitations, secret) {
			t.Fatalf("limitation exposed tool content %q: %s", secret, limitations)
		}
	}
}
