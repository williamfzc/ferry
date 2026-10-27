package session

import (
	"bytes"
	"strings"
	"testing"
)

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
