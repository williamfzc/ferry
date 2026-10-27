package session

import (
	"bytes"
	"strings"
	"testing"
)

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

func TestOpenCodeNullToolResultIsRetained(t *testing.T) {
	result, ok := openCodeToolResult("call-null", openCodeToolState{
		Status: "completed", Output: []byte("null"),
	})
	if !ok || result == nil || result.Content != "null" || string(result.RawContent) != "null" {
		t.Fatalf("OpenCode dropped explicit null output: ok=%v result=%+v", ok, result)
	}
}
