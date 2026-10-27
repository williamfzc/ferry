package session

import (
	"bytes"
	"encoding/json"
	"reflect"
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
`)
	source, err := (Codex{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), `Tool call "call-unknown" source status "queued_for_retry" has no ACP equivalent`) {
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
	if got := toolResultOrder(readBack); !sameStringSlice(got, []string{"a", "b"}) {
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
