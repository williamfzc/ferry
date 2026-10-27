package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestPiReadUsesActiveBranchAndPreservesOriginal(t *testing.T) {
	original := []byte("{" +
		`"type":"session","version":3,"id":"pi-session","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"` + "}\r\n" +
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"start"}}` + "\r\n" +
		`{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"thinking","thinking":"private"},{"type":"toolCall","id":"call-1","name":"read","arguments":{"path":"a","large":9007199254740993}}]}}` + "\r\n" +
		`{"type":"message","id":"r1","parentId":"a1","timestamp":"2026-01-01T00:00:03Z","message":{"role":"toolResult","toolCallId":"call-1","toolName":"read","content":[{"type":"text","text":"ok"}],"isError":false}}` + "\r\n" +
		`{"type":"message","id":"branch","parentId":"u1","timestamp":"2026-01-01T00:00:04Z","message":{"role":"user","content":"alternate"}}` + "\r\n")
	s, err := (Pi{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s.Original, original) {
		t.Fatal("Pi reader did not retain exact original bytes")
	}
	if s.SourceType != "pi" || s.NativeID != "pi-session" || s.Workspace != "/work" || len(s.Messages) != 2 {
		t.Fatalf("unexpected session identity/messages: %+v", s)
	}
	if s.Messages[0].Parts[0].Text != "start" || piTestText(s.Messages[1]) != "alternate" {
		t.Fatalf("reader did not select the active branch: %+v", s.Messages)
	}
}

func piTestText(m Message) string {
	if len(m.Parts) == 0 {
		return ""
	}
	return m.Parts[0].Text
}

func TestPiReadMapsToolCallAndResultWithoutFloatRounding(t *testing.T) {
	data := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","id":"a","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"c","name":"read","arguments":{"n":9007199254740993}}]}}` + "\n" +
		`{"type":"message","id":"r","parentId":"a","timestamp":"2026-01-01T00:00:02Z","message":{"role":"toolResult","toolCallId":"c","toolName":"read","content":[{"type":"text","text":"done"}],"isError":true}}` + "\n")
	s, err := (Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	call := s.Messages[0].Parts[0].ToolCall
	if call == nil || string(call.Input) != `{"n":9007199254740993}` {
		t.Fatalf("tool arguments changed: %+v", call)
	}
	result := s.Messages[1].Parts[0].ToolResult
	if result == nil || result.ID != "c" || result.Content != "done" || !result.IsError {
		t.Fatalf("tool result changed: %+v", result)
	}
}

func TestPiReadAppliesCompactionContext(t *testing.T) {
	data := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"old"}}` + "\n" +
		`{"type":"message","id":"u2","parentId":"u1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"user","content":"kept"}}` + "\n" +
		`{"type":"compaction","id":"c","parentId":"u2","timestamp":"2026-01-01T00:00:03Z","summary":"summary","firstKeptEntryId":"u2"}` + "\n" +
		`{"type":"message","id":"u3","parentId":"c","timestamp":"2026-01-01T00:00:04Z","message":{"role":"user","content":"new"}}` + "\n")
	s, err := (Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || piTestText(s.Messages[0]) != "summary" || piTestText(s.Messages[1]) != "kept" || piTestText(s.Messages[2]) != "new" {
		t.Fatalf("compaction context is wrong: %+v", s.Messages)
	}
}

func TestPiReadContinuousCompactionsDropOlderSummaries(t *testing.T) {
	data := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","id":"old","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"old"}}` + "\n" +
		`{"type":"compaction","id":"s1","parentId":"old","timestamp":"2026-01-01T00:00:02Z","summary":"s1","firstKeptEntryId":"old"}` + "\n" +
		`{"type":"message","id":"kept","parentId":"s1","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"kept"}}` + "\n" +
		`{"type":"compaction","id":"s2","parentId":"kept","timestamp":"2026-01-01T00:00:04Z","summary":"s2","firstKeptEntryId":"old"}` + "\n" +
		`{"type":"message","id":"new","parentId":"s2","timestamp":"2026-01-01T00:00:05Z","message":{"role":"user","content":"new"}}` + "\n")
	s, err := (Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 4 || piTestText(s.Messages[0]) != "s2" || piTestText(s.Messages[1]) != "old" || piTestText(s.Messages[2]) != "kept" || piTestText(s.Messages[3]) != "new" {
		t.Fatalf("older compaction was not removed from active context: %+v", s.Messages)
	}
}

func TestPiReadMigratesLegacyCompactionIndex(t *testing.T) {
	data := []byte(`{"type":"session","version":1,"id":"legacy","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"old"}}` + "\n" +
		`{"type":"compaction","timestamp":"2026-01-01T00:00:02Z","summary":"summary","firstKeptEntryIndex":0}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"new"}}` + "\n")
	s, err := (Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || piTestText(s.Messages[0]) != "summary" || piTestText(s.Messages[1]) != "new" {
		t.Fatalf("legacy compaction index was not applied: %+v", s.Messages)
	}
}

func TestPiReadRejectsContextChangingUnsupportedEntries(t *testing.T) {
	for _, entry := range []string{
		`{"type":"context_edit","id":"x","parentId":null,"timestamp":"2026-01-01T00:00:01Z","targetId":"u","replacement":null}`,
		`{"type":"custom_message","id":"x","parentId":null,"timestamp":"2026-01-01T00:00:01Z","customType":"ext","content":"hidden","display":false}`,
		`{"type":"message","id":"x","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"bashExecution","command":"pwd","output":"/work"}}`,
		`{"type":"message","id":"x","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":[{"type":"toolCall","id":"c","name":"run","arguments":{}}]}}`,
		`{"type":"message","id":"x","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}]}}`,
	} {
		data := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" + entry + "\n")
		if _, err := (Pi{}).Read(bytes.NewReader(data)); err == nil {
			t.Fatalf("accepted unsupported context entry: %s", entry)
		}
	}
}

func TestPiReadRejectsInvalidTimestamps(t *testing.T) {
	badHeader := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"not-a-time","cwd":"/work"}` + "\n")
	if _, err := (Pi{}).Read(bytes.NewReader(badHeader)); err == nil {
		t.Fatal("accepted invalid Pi header timestamp")
	}
	badEntry := []byte(`{"type":"session","version":3,"id":"pi","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","id":"x","parentId":null,"timestamp":"not-a-time","message":{"role":"user","content":"x"}}` + "\n")
	if _, err := (Pi{}).Read(bytes.NewReader(badEntry)); err == nil {
		t.Fatal("accepted invalid Pi entry timestamp")
	}
}

func TestPiWriteReadSupportedSubset(t *testing.T) {
	source := Session{
		NativeID:  "pi-written",
		Workspace: "/work",
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "text", Text: "reading"},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"x","n":9007199254740993}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "call-1", Content: "ok", IsError: true}}}},
		},
	}
	var out bytes.Buffer
	if err := (Pi{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"version":3`)) || !bytes.Contains(out.Bytes(), []byte(`"role":"toolResult"`)) {
		t.Fatalf("output is not Pi JSONL: %s", out.Bytes())
	}
	got, err := (Pi{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != source.NativeID || got.Workspace != source.Workspace || len(got.Messages) != 3 {
		t.Fatalf("identity/messages changed: %+v", got)
	}
	if err := validateProjection(source, got); err != nil {
		t.Fatalf("supported projection changed: %v", err)
	}
	if !got.Messages[2].Parts[0].ToolResult.IsError {
		t.Fatal("Pi tool result error flag did not round-trip")
	}
	if !bytes.Contains(out.Bytes(), []byte(`"toolName":"read"`)) {
		t.Fatalf("Pi output did not retain the matching tool name: %s", out.Bytes())
	}
}

func TestPiWriteCompactionHasRequiredFieldsAndReadsContinuousSummaries(t *testing.T) {
	source := Session{NativeID: "pi-summary", Messages: []Message{
		{Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "first"}}},
		{Role: "user", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "text", Text: "after"}}},
	}}
	var out bytes.Buffer
	if err := (Pi{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	var lines []map[string]any
	for _, line := range bytes.Split(bytes.TrimSpace(out.Bytes()), []byte{'\n'}) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		lines = append(lines, record)
	}
	record := lines[1]
	if record["type"] != "compaction" || record["firstKeptEntryId"] == nil || record["tokensBefore"] == nil {
		t.Fatalf("incomplete compaction record: %+v", record)
	}
	if _, err := (Pi{}).Read(bytes.NewReader(out.Bytes())); err != nil {
		t.Fatal(err)
	}
}

func TestPiWriteRejectsSummaryAfterMessages(t *testing.T) {
	source := Session{NativeID: "pi-summary", Messages: []Message{
		{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "before"}}},
		{Role: "summary", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "text", Text: "summary"}}},
	}}
	var out bytes.Buffer
	if err := (Pi{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "summary after messages") || out.Len() != 0 {
		t.Fatalf("summary reordering was accepted or partially written: err=%v output=%q", err, out.String())
	}
}

func TestPiWriteRejectsConsecutiveSummaries(t *testing.T) {
	source := Session{NativeID: "pi-summary", Messages: []Message{
		{Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "first"}}},
		{Role: "summary", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "text", Text: "second"}}},
	}}
	var out bytes.Buffer
	if err := (Pi{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "consecutive summaries") || out.Len() != 0 {
		t.Fatalf("consecutive summaries were accepted or partially written: err=%v output=%q", err, out.String())
	}
}

func TestPiWriteRejectsMultiPartSummaryWithoutOutput(t *testing.T) {
	source := Session{NativeID: "pi-summary", Messages: []Message{{
		Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "first"}, {Type: "text", Text: "second"}},
	}}}
	var out bytes.Buffer
	if err := (Pi{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "exactly one text part") || out.Len() != 0 {
		t.Fatalf("multi-part summary was accepted or partially written: err=%v output=%q", err, out.String())
	}
}

func TestPiWriteSupportsNativeAndClaudeImages(t *testing.T) {
	images := []ContentBlock{
		{Type: "image", Raw: json.RawMessage(`{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}`)},
		{Type: "image", Raw: json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"aGVsbG8="}}`)},
	}
	for _, image := range images {
		var out bytes.Buffer
		s := Session{NativeID: "pi", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{image}}}}
		if err := (Pi{}).Write(&out, s); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), `"type":"image"`) || !strings.Contains(out.String(), `"mimeType"`) {
			t.Fatalf("image was not written natively: %s", out.String())
		}
	}
}

func TestPiWriteRejectsUnrepresentableContentWithoutOutput(t *testing.T) {
	cases := []Session{
		{NativeID: "bad id", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "x"}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "unknown", Raw: json.RawMessage(`{"future":true}`)}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_call", ToolCall: &ToolCall{ID: "c", Name: "run", Status: "completed", Input: json.RawMessage(`{}`)}}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "input_image", Raw: json.RawMessage(`{"type":"input_image","image_url":"https://example.test/x.png"}`)}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "tool", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "missing", Content: "x"}}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_call", ToolCall: &ToolCall{ID: "c", Name: "run", Input: json.RawMessage(`{}`)}}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "image", Raw: json.RawMessage(`{"type":"image","data":"aGVsbG8=","mimeType":"image/png"}`)}}}}},
		{NativeID: "pi", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "input_image", Raw: json.RawMessage(`{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}`)}}}}},
	}
	for _, source := range cases {
		var out bytes.Buffer
		if err := (Pi{}).Write(&out, source); err == nil || out.Len() != 0 {
			t.Fatalf("Pi accepted or partially wrote unsupported content: err=%v output=%q", err, out.String())
		}
	}
}

func TestPiReadMigratesLegacyLinearEntries(t *testing.T) {
	data := []byte(`{"type":"session","version":1,"id":"legacy","timestamp":"2026-01-01T00:00:00Z","cwd":"/work"}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"one"}}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":"two"}}` + "\n")
	s, err := (Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[1].Parts[0].Text != "two" {
		t.Fatalf("legacy entries were not read linearly: %+v", s.Messages)
	}
}

func TestPiConvertAcceptsForeignStatusAndFreeText(t *testing.T) {
	source := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/example"}}` + "\n" + `{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"custom_tool_call","call_id":"c1","name":"run","input":"hello","status":"completed"}}` + "\n" + `{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"done"}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "pi")
	if err != nil {
		t.Fatal(err)
	}
	notes := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(notes, "free-text") || !strings.Contains(notes, "status") {
		t.Fatalf("missing conversion limitations: %v", transfer.Limitations)
	}
}
