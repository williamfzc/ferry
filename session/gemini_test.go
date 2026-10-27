package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGeminiReadPreservesJSONLAndAppliesPatches(t *testing.T) {
	original := []byte("{" +
		`"sessionId":"gem-session","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:03Z"` + "}\r\n" +
		`{"id":"u1","type":"user","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"hello"}]}` + "\r\n" +
		`{"id":"a1","type":"gemini","timestamp":"2026-01-01T00:00:02Z","content":[{"text":"checking"}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"path":"x"},"status":"success","timestamp":"2026-01-01T00:00:02Z"}]}` + "\r\n" +
		`{"$patch":{"updates":[{"id":"a1","content":[{"text":"done"}],"toolCalls":[{"id":"call-1","result":[{"text":"ok"}]}]}]}}` + "\r\n" +
		`{"id":"u2","type":"user","timestamp":"2026-01-01T00:00:03Z","content":[{"text":"discard me"}]}` + "\r\n" +
		`{"$rewindTo":"u2"}` + "\r\n" +
		`{"future":"record"}`)

	s, err := (Gemini{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	if s.SourceType != "gemini" || s.NativeID != "gem-session" || len(s.Messages) != 3 {
		t.Fatalf("unexpected session metadata/messages: %+v", s)
	}
	if !bytes.Equal(s.Original, original) {
		t.Fatal("reader did not retain exact original bytes")
	}
	if s.Messages[0].Role != "user" || s.Messages[0].Parts[0].Text != "hello" {
		t.Fatalf("unexpected user message: %+v", s.Messages[0])
	}
	assistant := s.Messages[1]
	if assistant.Role != "assistant" || assistant.Parts[0].Text != "done" || assistant.Parts[1].ToolCall == nil {
		t.Fatalf("tool call was not represented: %+v", assistant)
	}
	result := s.Messages[2]
	if result.Role != "tool" || result.Parts[0].ToolResult == nil || result.Parts[0].ToolResult.Content != "ok" || string(result.Parts[0].ToolResult.RawContent) != `[{"text":"ok"}]` {
		t.Fatalf("tool result was not split into its own message: %+v", result)
	}
	if !s.Updated.Equal(time.Date(2026, 1, 1, 0, 0, 3, 0, time.UTC)) {
		t.Fatalf("unexpected update time: %v", s.Updated)
	}
}

func TestGeminiReadLegacyJSON(t *testing.T) {
	data := []byte(`{"sessionId":"legacy","projectHash":"hash","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:01Z","messages":[{"id":"u","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"hello"},{"id":"g","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"world"},{"inlineData":{"mimeType":"image/png","data":"aA=="}}]}]}`)
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].Text != "hello" || s.Messages[1].Parts[1].Type != "image" {
		t.Fatalf("legacy record not parsed: %+v", s.Messages)
	}
}

func TestGeminiReadDoesNotDuplicateFunctionCallMetadata(t *testing.T) {
	data := []byte(`{"sessionId":"dup","projectHash":"hash","messages":[]}` + "\n" +
		`{"id":"a","type":"gemini","timestamp":"2026-01-01T00:00:00Z","content":[{"functionCall":{"id":"call-1","name":"read_file","args":{"path":"x"}}}],"toolCalls":[{"id":"call-1","name":"read_file","args":{"path":"x"},"status":"success"}]}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 1 || s.Messages[0].Parts[0].Type != "tool_call" {
		t.Fatalf("function call was duplicated: %+v", s.Messages)
	}
	if got := s.Messages[0].Parts[0].ToolCall.Status; got != "completed" {
		t.Fatalf("Gemini success status = %q, want completed", got)
	}
	if geminiToolStatus(s.Messages[0].Parts[0].Raw) != "success" {
		t.Fatalf("explicit tool status was lost while merging: %+v", s.Messages[0].Parts[0])
	}
	s.Workspace = "/work"
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, s); err != nil || !strings.Contains(out.String(), `"status":"success"`) {
		t.Fatalf("explicit status did not round-trip: err=%v output=%s", err, out.String())
	}
}

func TestGeminiReadMapsKnownStatusesAndPreservesUnknown(t *testing.T) {
	data := []byte(`{"sessionId":"status","messages":[]}` + "\n" +
		`{"id":"a","type":"gemini","timestamp":"2026-01-01T00:00:00Z","content":[],"toolCalls":[{"id":"run","name":"run","args":{},"status":"executing"},{"id":"later","name":"run","args":{},"status":"cancelled"}]}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 2 {
		t.Fatalf("unexpected Gemini calls: %+v", s.Messages)
	}
	if got := s.Messages[0].Parts[0].ToolCall.Status; got != "in_progress" {
		t.Fatalf("executing status = %q, want in_progress", got)
	}
	unknown := s.Messages[0].Parts[1]
	if unknown.ToolCall.Status != "" || !strings.Contains(string(unknown.Raw), `"status":"cancelled"`) {
		t.Fatalf("non-ACP Gemini status was guessed or discarded: %+v", unknown)
	}
}

func TestGeminiErrorStatusMarksToolResult(t *testing.T) {
	data := []byte(`{"sessionId":"error-status","messages":[]}` + "\n" +
		`{"id":"a","type":"gemini","timestamp":"2026-01-01T00:00:00Z","content":[],"toolCalls":[{"id":"failed","name":"run","args":{},"status":"error","result":[{"text":"nope"}]}]}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || len(s.Messages[1].Parts) != 1 || s.Messages[1].Parts[0].ToolResult == nil {
		t.Fatalf("Gemini error result was not split: %+v", s.Messages)
	}
	if !s.Messages[1].Parts[0].ToolResult.IsError {
		t.Fatalf("Gemini error status did not mark the tool result: %+v", s.Messages[1].Parts[0])
	}
}

func TestGeminiErrorStatusMarksInlineToolResult(t *testing.T) {
	data := []byte(`{"sessionId":"inline-error","messages":[]}` + "\n" +
		`{"id":"a","type":"gemini","timestamp":"2026-01-01T00:00:00Z","content":[{"functionResponse":{"id":"failed","name":"run","response":{"text":"nope"}}}],"toolCalls":[{"id":"failed","name":"run","args":{},"status":"error"}]}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 2 || s.Messages[0].Parts[0].ToolResult == nil || !s.Messages[0].Parts[0].ToolResult.IsError {
		t.Fatalf("inline Gemini error status did not mark the tool result: %+v", s.Messages)
	}
}

func TestGeminiReadSupportsSinglePartAndFileDataImage(t *testing.T) {
	data := []byte(`{"sessionId":"parts","projectHash":"hash","messages":[]}` + "\n" +
		`{"id":"u","type":"user","timestamp":"2026-01-01T00:00:00Z","content":{"text":"single"}}` + "\n" +
		`{"id":"g","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":{"fileData":{"mimeType":"image/png","fileUri":"file:///tmp/x.png"}}}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].Text != "single" || s.Messages[1].Parts[0].Type != "image" {
		t.Fatalf("single part or fileData image was not read: %+v", s.Messages)
	}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, Session{NativeID: "parts", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "image", Raw: s.Messages[1].Parts[0].Raw}}}}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"fileData"`) {
		t.Fatalf("fileData image was not written: %s", out.String())
	}
}

func TestGeminiWriteReadSupportedSubset(t *testing.T) {
	source := Session{
		NativeID:  "write-session",
		Workspace: "/work/project",
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "text", Text: "reading"},
				{Type: "tool_call", Raw: json.RawMessage(`{"id":"call-1","status":"success"}`), ToolCall: &ToolCall{ID: "call-1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "call-1", Content: "ok", RawContent: json.RawMessage(`[{"text":"ok"}]`)}},
			}},
		},
	}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"projectHash"`) || !strings.Contains(out.String(), `"toolCalls"`) {
		t.Fatalf("output is not Gemini JSONL: %s", out.String())
	}
	got, err := (Gemini{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 3 || got.Messages[1].Parts[1].ToolCall == nil || got.Messages[2].Role != "tool" || got.Messages[2].Parts[0].ToolResult == nil {
		t.Fatalf("written subset did not read back: %+v", got.Messages)
	}
}

func TestGeminiWriteReadPreservesToolResultTimestamp(t *testing.T) {
	source := Session{
		NativeID:  "timestamp-session",
		Workspace: "/work/project",
		Messages: []Message{
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{
				Type:     "tool_call",
				ToolCall: &ToolCall{ID: "call-1", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)},
			}}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{{
				Type:       "tool_result",
				ToolResult: &ToolResult{ID: "call-1", Content: "ok"},
			}}},
		},
	}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"resultTimestamp":"2026-01-01T00:00:02Z"`) {
		t.Fatalf("tool result timestamp was not recorded: %s", out.String())
	}
	got, err := (Gemini{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) != 2 || got.Messages[1].Role != "tool" || got.Messages[1].Timestamp != "2026-01-01T00:00:02Z" {
		t.Fatalf("tool result timestamp changed on read-back: %+v", got.Messages)
	}
}

func TestGeminiWriteRejectsUnsupportedWithoutOutput(t *testing.T) {
	tests := []Session{
		{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "tool", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "not representable"}}}}},
		{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "unknown", Raw: json.RawMessage(`{"future":true}`)}}}}},
		{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "summary"}}}}},
	}
	for _, source := range tests {
		var out bytes.Buffer
		if err := (Gemini{}).Write(&out, source); err == nil || out.Len() != 0 {
			t.Fatalf("unsupported session was accepted or partially written: err=%v output=%q", err, out.String())
		}
	}
}

func TestGeminiWriteRequiresWorkspace(t *testing.T) {
	source := Session{NativeID: "id", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "workspace") || out.Len() != 0 {
		t.Fatalf("missing workspace was not rejected: err=%v output=%q", err, out.String())
	}
}

func TestGeminiWriteRejectsUnknownToolStatus(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "refusing to claim success") || out.Len() != 0 {
		t.Fatalf("dangling call outcome was silently claimed: err=%v output=%q", err, out.String())
	}
}

// A paired result is itself evidence of the outcome: success unless the
// result is flagged as an error.
func TestGeminiWriteDerivesToolStatusFromResult(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{
		{Type: "tool_call", ToolCall: &ToolCall{ID: "call", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}},
		{Type: "tool_result", ToolResult: &ToolResult{ID: "call", Content: "ok"}},
	}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"status":"success"`) || !strings.Contains(out.String(), `"result"`) {
		t.Fatalf("paired result was not written with derived status: %s", out.String())
	}
}

func TestGeminiWriteSupportsUserFunctionResponse(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", Raw: json.RawMessage(`{"functionResponse":{"id":"call","name":"read_file","response":{"output":"ok"}}}`), ToolResult: &ToolResult{ID: "call", RawContent: json.RawMessage(`{"output":"ok"}`)}}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"functionResponse"`) {
		t.Fatalf("user function response was not written: %s", out.String())
	}
	got, err := (Gemini{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil || len(got.Messages) != 1 || got.Messages[0].Parts[0].ToolResult == nil || string(got.Messages[0].Parts[0].ToolResult.RawContent) != `{"output":"ok"}` {
		t.Fatalf("user function response did not read back: err=%v session=%+v", err, got)
	}
}

func TestGeminiWriteRejectsFunctionResponseWithoutName(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "call", Content: "ok"}}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "no earlier call") || out.Len() != 0 {
		t.Fatalf("orphan result was invented a call: err=%v output=%q", err, out.String())
	}
}

func TestGeminiWritePreservesFunctionResponseParts(t *testing.T) {
	response := json.RawMessage(`{"output":"ok"}`)
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", Raw: json.RawMessage(`{"functionResponse":{"id":"call","name":"read_file","response":{"output":"ok"},"parts":[{"fileData":{"mimeType":"image/png","fileUri":"file:///tmp/x.png"}}]}}`), ToolResult: &ToolResult{ID: "call", RawContent: response}}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err != nil || !bytes.Contains(out.Bytes(), []byte(`"parts":[{"fileData"`)) {
		t.Fatalf("function response parts were dropped: err=%v output=%s", err, out.String())
	}
	got, err := (Gemini{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil || len(got.Messages) != 1 || !bytes.Contains(got.Messages[0].Parts[0].Raw, []byte(`"parts":[{"fileData"`)) {
		t.Fatalf("top-level function response parts did not round-trip: err=%v session=%+v", err, got)
	}
}

func TestGeminiWriteRejectsThoughtSignatureLoss(t *testing.T) {
	for _, part := range []ContentBlock{
		{Type: "text", Raw: json.RawMessage(`{"text":"answer","thoughtSignature":"sig"}`), Text: "answer"},
		{Type: "tool_call", Raw: json.RawMessage(`{"functionCall":{"id":"call","name":"read_file","args":{},"thought_signature":"sig"}}`), ToolCall: &ToolCall{ID: "call", Name: "read_file", Input: json.RawMessage(`{}`)}},
	} {
		source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{part}}}}
		var out bytes.Buffer
		if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "thought signature") || out.Len() != 0 {
			t.Fatalf("thought signature was silently dropped: err=%v output=%q", err, out.String())
		}
	}
}

func TestGeminiWriteRejectsArbitraryToolStatus(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{
		{Type: "tool_call", Raw: json.RawMessage(`{"status":"mystery"}`), ToolCall: &ToolCall{ID: "call", Name: "read_file", Input: json.RawMessage(`{"path":"x"}`)}},
		{Type: "tool_result", ToolResult: &ToolResult{ID: "call", Content: "ok"}},
	}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "unsupported tool status") || out.Len() != 0 {
		t.Fatalf("arbitrary tool status was accepted: err=%v output=%q", err, out.String())
	}
}

func TestGeminiWriteRejectsDuplicateToolCallIDs(t *testing.T) {
	source := Session{
		NativeID:  "duplicate-call-session",
		Workspace: "/work",
		Messages: []Message{
			{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{
				Type:     "tool_call",
				ToolCall: &ToolCall{ID: "call-1", Name: "first", Input: json.RawMessage(`{}`)},
			}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{
				Type:     "tool_call",
				ToolCall: &ToolCall{ID: "call-1", Name: "second", Input: json.RawMessage(`{}`)},
			}}},
		},
	}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("duplicate tool call id was accepted: err=%v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("duplicate tool call produced partial output: %q", out.String())
	}
}

func TestGeminiReadRejectsMalformedLine(t *testing.T) {
	data := []byte(`{"sessionId":"bad","projectHash":"hash"}` + "\n" + "{not-json}\n")
	if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "session:2") {
		t.Fatalf("malformed line was silently ignored: %v", err)
	}
}

func TestGeminiReadRejectsUnknownRewindTarget(t *testing.T) {
	data := []byte(`{"sessionId":"rewind","projectHash":"hash"}` + "\n" +
		`{"id":"u","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"keep"}` + "\n" +
		`{"$rewindTo":"missing"}` + "\n")
	if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "target") {
		t.Fatalf("unknown rewind target was accepted: %v", err)
	}
}

func TestGeminiReadRejectsMalformedStateRecords(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
	}{
		{name: "set", line: `{"$set":null}`, want: "invalid $set"},
		{name: "set messages", line: `{"$set":{"messages":"bad"}}`, want: "invalid $set.messages"},
		{name: "patch", line: `{"$patch":null}`, want: "invalid $patch"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"sessionId":"state","projectHash":"hash"}` + "\n" + tc.line + "\n")
			if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("malformed state record was accepted: err=%v", err)
			}
		})
	}
}

func TestGeminiReadRejectsMessageWithoutID(t *testing.T) {
	data := []byte(`{"sessionId":"missing-id","projectHash":"hash"}` + "\n" +
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","content":"hello"}` + "\n")
	if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "has no id") {
		t.Fatalf("message without id was silently dropped: %v", err)
	}
}

func TestGeminiReadPreservesUnknownRecordWithoutID(t *testing.T) {
	data := []byte(`{"sessionId":"unknown-record","projectHash":"hash"}` + "\n" +
		`{"type":"future_event","payload":{"value":1}}` + "\n")
	s, err := (Gemini{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("unknown record without id was rejected: %v", err)
	}
	if len(s.Messages) != 0 {
		t.Fatalf("unknown record was converted into a message: %+v", s.Messages)
	}
	if !bytes.Equal(s.Original, data) {
		t.Fatalf("unknown record was not retained in the original source")
	}
}

func TestGeminiReadRejectsPatchReferenceToMissingMessage(t *testing.T) {
	for _, patch := range []string{
		`{"$patch":{"updates":[{"id":"missing","content":"x"}]}}`,
		`{"$patch":{"removeIds":["missing"]}}`,
		`{"$patch":{"orderIds":["missing"]}}`,
	} {
		data := []byte(`{"sessionId":"missing-patch","projectHash":"hash"}` + "\n" + patch + "\n")
		if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "nonexistent message") {
			t.Fatalf("missing patch reference was silently ignored: patch=%s err=%v", patch, err)
		}
	}
}

func TestGeminiRejectsArrayFunctionResponse(t *testing.T) {
	data := []byte(`{"sessionId":"bad-response","projectHash":"hash","messages":[]}` + "\n" +
		`{"id":"u","type":"user","timestamp":"2026-01-01T00:00:00Z","content":[{"functionResponse":{"id":"call","name":"read_file","response":[{"text":"ok"}]}}]}` + "\n")
	if _, err := (Gemini{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "response must be a JSON object") {
		t.Fatalf("array function response was accepted: %v", err)
	}
}

func TestGeminiWriteRejectsArrayFunctionResponse(t *testing.T) {
	source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", Raw: json.RawMessage(`{"functionResponse":{"id":"call","name":"read_file","response":[{"text":"ok"}]}}`), ToolResult: &ToolResult{ID: "call", RawContent: json.RawMessage(`[{"text":"ok"}]`)}}}}}}
	var out bytes.Buffer
	if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "must be a JSON object") || out.Len() != 0 {
		t.Fatalf("array function response was written: err=%v output=%q", err, out.String())
	}
}

func TestGeminiWriteRejectsUnsupportedFunctionResponseFields(t *testing.T) {
	for _, field := range []string{"scheduling", "willContinue", "futureField"} {
		raw := json.RawMessage(`{"functionResponse":{"id":"call","name":"read_file","response":{"output":"ok"},"` + field + `":true}}`)
		source := Session{NativeID: "id", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", Raw: raw, ToolResult: &ToolResult{ID: "call", RawContent: json.RawMessage(`{"output":"ok"}`)}}}}}}
		var out bytes.Buffer
		if err := (Gemini{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "unsupported function response field") || out.Len() != 0 {
			t.Fatalf("unsupported function response field %q was dropped: err=%v output=%q", field, err, out.String())
		}
	}
}
