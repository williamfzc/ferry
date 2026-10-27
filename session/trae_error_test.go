package session

import (
	"bytes"
	"strings"
	"testing"
)

func TestTraeWriteReadPreservesToolResultError(t *testing.T) {
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174000",
		Workspace: "/work",
		Messages: []Message{{
			Role:      "tool",
			Timestamp: "2026-01-01T00:00:00Z",
			Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{
				ID: "call-1", Content: "failed", IsError: true,
			}}},
		}},
	}

	var output bytes.Buffer
	if err := (Trae{}).Write(&output, source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"is_error":true`) {
		t.Fatalf("Trae output dropped the tool error flag: %s", output.String())
	}

	got, err := (Trae{}).Read(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	result := got.Messages[0].Parts[0].ToolResult
	if result == nil || !result.IsError {
		t.Fatalf("Trae read-back dropped the tool error flag: %+v", got.Messages)
	}
}

func TestConvertToTraePreservesToolResultError(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"failed","is_error":true}]}}` + "\n")
	source, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}

	transfer, err := Convert(source, "trae")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(transfer.Limitations, "\n"), "flag is dropped") {
		t.Fatalf("preserved error incorrectly reported lost: %v", transfer.Limitations)
	}
	if !strings.Contains(string(transfer.Target), `"is_error":true`) {
		t.Fatalf("Trae conversion dropped the tool error flag: %s", transfer.Target)
	}
	got, err := (Trae{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	result := got.Messages[0].Parts[0].ToolResult
	if result == nil || !result.IsError {
		t.Fatalf("Trae conversion read-back dropped the tool error flag: %+v", got.Messages)
	}
}
