package session

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func TestTraeReadCanonicalHistoryMutation(t *testing.T) {
	data := []byte(`{"timestamp":"2026-10-27T00:00:00Z","type":"session_meta","payload":{"id":"01a0c823-27eb-71c2-9d91-fce2ba3fb37a","timestamp":"2026-10-27T00:00:00Z","cwd":"/work/project","originator":"traex-tui","cli_version":"0.210.1","source":"cli","model_provider":"trae"}}` + "\n" +
		`{"timestamp":"2026-10-27T00:00:01Z","type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"u1","role":"user","content":[{"type":"input_text","text":"read file"}]},{"type":"message","id":"a1","role":"assistant","content":[{"type":"output_text","text":"Checking"}]},{"type":"function_call","id":"fc1","call_id":"fc1","name":"Read","arguments":"{\"path\":\"a.go\"}"},{"type":"function_call_output","id":"fco1","call_id":"fc1","output":"contents"}]}}` + "\n" +
		`{"timestamp":"2026-10-27T00:00:02Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"text","text":"read file"}]}}}` + "\n")

	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if s.SourceType != "trae" || s.NativeID == "" || s.Workspace != "/work/project" {
		t.Fatalf("unexpected identity: %+v", s)
	}
	if !bytes.Equal(s.Original, data) {
		t.Fatal("original bytes were not retained")
	}
	if len(s.Messages) != 4 {
		t.Fatalf("expected canonical user, assistant, call and result; got %d: %+v", len(s.Messages), s.Messages)
	}
	if s.Messages[0].Role != "user" || s.Messages[0].Parts[0].Text != "read file" {
		t.Fatalf("unexpected user message: %+v", s.Messages[0])
	}
	call := s.Messages[2].Parts[0].ToolCall
	if s.Messages[2].Role != "assistant" || call == nil || call.ID != "fc1" || call.Name != "Read" || !strings.Contains(string(call.Input), "a.go") {
		t.Fatalf("unexpected tool call: %+v", s.Messages[2])
	}
	result := s.Messages[3].Parts[0].ToolResult
	if s.Messages[3].Role != "tool" || result == nil || result.ID != "fc1" || result.Content != "contents" {
		t.Fatalf("unexpected tool result: %+v", s.Messages[3])
	}
}

func TestTraeReadRealInputImageAlongsideText(t *testing.T) {
	const image = `{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}`
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"message","id":"u1","role":"user","content":[{"type":"input_text","text":"describe this image"},` + image + `]}]}}` + "\n")

	source, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(source.Messages) != 1 || len(source.Messages[0].Parts) != 2 {
		t.Fatalf("expected one user message with text and image, got %+v", source.Messages)
	}
	if source.Messages[0].Parts[0].Type != "text" || source.Messages[0].Parts[0].Text != "describe this image" {
		t.Fatalf("text part was not read: %+v", source.Messages[0].Parts[0])
	}
	imagePart := source.Messages[0].Parts[1]
	if imagePart.Type != "input_image" || string(imagePart.Raw) != image {
		t.Fatalf("input image was not recognized or its raw block changed: %+v", imagePart)
	}
	if !bytes.Equal(source.Original, data) {
		t.Fatal("Trae original bytes were not retained")
	}

	for _, target := range []string{"claude", "codex"} {
		t.Run(target, func(t *testing.T) {
			transfer, err := Convert(source, target)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(transfer.Original, data) {
				t.Fatal("conversion did not retain the exact Trae source")
			}
			var converted Session
			switch target {
			case "claude":
				converted, err = (Claude{}).Read(bytes.NewReader(transfer.Target))
			case "codex":
				converted, err = (Codex{}).Read(bytes.NewReader(transfer.Target))
			}
			if err != nil {
				t.Fatal(err)
			}
			var convertedParts []ContentBlock
			for _, message := range converted.Messages {
				convertedParts = append(convertedParts, message.Parts...)
			}
			if len(convertedParts) != 2 || convertedParts[0].Type != "text" || convertedParts[0].Text != "describe this image" {
				t.Fatalf("converted text/image content changed shape: %+v", converted.Messages)
			}
			convertedImage := convertedParts[1]
			if target == "claude" && convertedImage.Type != "image" {
				t.Fatalf("Claude did not use its native image representation: %+v", convertedImage)
			}
			if target == "codex" && convertedImage.Type != "input_image" {
				t.Fatalf("Codex did not use its native input image representation: %+v", convertedImage)
			}
		})
	}
}

func TestTraeReadFallsBackToResponseItemsAndEvents(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"user_message","message":"hello"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"world"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"function_call","call_id":"call-1","name":"run","arguments":"{}","status":"completed"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:04Z","payload":{"type":"function_call_output","call_id":"call-1","output":"ok"}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("unexpected fallback messages: %+v", s.Messages)
	}
	if s.Messages[0].Role != "user" || s.Messages[1].Parts[0].Text != "world" || s.Messages[2].Parts[0].ToolCall.Status != "completed" || s.Messages[3].Parts[0].ToolResult.Content != "ok" {
		t.Fatalf("fallback records not mapped: %+v", s.Messages)
	}
}

func TestTraeFallbackDeduplicatesResponseItemAndCompletedEvent(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","id":"same-prompt","role":"user","content":[{"type":"input_text","text":"same prompt"}]}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:01.500Z","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"same-prompt","content":[{"type":"text","text":"same prompt"}]}}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","id":"same-answer","role":"assistant","content":[{"type":"output_text","text":"same answer"}]}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:02.500Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"same-answer","content":[{"type":"Text","text":"same answer"}]}}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].Text != "same prompt" || s.Messages[1].Parts[0].Text != "same answer" {
		t.Fatalf("duplicate UI events were not collapsed: %+v", s.Messages)
	}
}

func TestTraeReadPreservesFunctionCallOutputError(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"function_call","call_id":"call-1","name":"run","arguments":"{}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"function_call_output","call_id":"call-1","output":"failed","is_error":true}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[1].Parts[0].ToolResult == nil || !s.Messages[1].Parts[0].ToolResult.IsError {
		t.Fatalf("function_call_output error flag was lost: %+v", s.Messages)
	}
}

func TestTraeRepeatedTextWithoutSharedIdentityIsNotDropped(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"repeat"}]}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:01.500Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"different-turn","content":[{"type":"text","text":"repeat"}]}}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 {
		t.Fatalf("same text without shared identity was dropped: %+v", s.Messages)
	}
}

func TestTraeDisplayCompletionsAndRuntimeContext(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<system-reminder>runtime context</system-reminder>"}]}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"u1","role":"user","content":[{"type":"input_text","text":"real prompt"}]}],"display_completions":[{"turn_id":"turn-1","item":{"type":"AgentMessage","id":"a1","content":[{"type":"Text","text":"visible answer"}]}}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].Text != "real prompt" || s.Messages[1].Parts[0].Text != "visible answer" {
		t.Fatalf("display completion/runtime context mapping wrong: %+v", s.Messages)
	}
}

func TestTraeCanonicalContextIsExcludedButQuotedMarkerRemains(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"runtime","role":"user","content":[{"type":"input_text","text":"<environment_context>injected</environment_context>"}]},` +
		`{"type":"message","id":"quote","role":"user","content":[{"type":"input_text","text":"Please explain the marker <environment_context> literally."}]},` +
		`{"type":"message","id":"instructions","role":"user","content":[{"type":"input_text","text":"# AGENTS.md instructions\n<INSTRUCTIONS>injected</INSTRUCTIONS>"}]}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "Please explain the marker <environment_context> literally." {
		t.Fatalf("synthetic context was projected or legitimate quote was dropped: %+v", s.Messages)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "synthetic_context=2") {
		t.Fatalf("excluded synthetic records were not reported: %v", transfer.Limitations)
	}
}

func TestTraeNestedToolErrorAndTextArray(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","payload":{"type":"function_call_output","call_id":"call-1","output":{"content":[{"type":"input_text","text":"first"},{"type":"image","text":"not text output"},{"type":"output_text","text":"second"}],"is_error":true}}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].ToolResult == nil {
		t.Fatalf("tool result missing: %+v", s.Messages)
	}
	result := s.Messages[0].Parts[0].ToolResult
	if result.Content != "first\nsecond" || !result.IsError {
		t.Fatalf("nested error/text projection wrong: %+v", result)
	}
}

func TestTraeReplaceReplacesEarlierHistory(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"old","role":"user","content":"old"}]}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"replace","items":[{"type":"message","id":"new","role":"user","content":"new"}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "new" {
		t.Fatalf("replace did not reset history: %+v", s.Messages)
	}
}

func TestTraeRollbackRemovesTargetTurn(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"old","role":"user","content":"old","internal_chat_message_metadata_passthrough":{"turn_id":"turn-old"}},{"type":"message","id":"keep","role":"user","content":"keep","internal_chat_message_metadata_passthrough":{"turn_id":"turn-keep"}}]}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"rollback","target_turn_id":"turn-old","num_turns":1}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "keep" {
		t.Fatalf("rollback did not remove target turn: %+v", s.Messages)
	}
}

func TestTraeUnsupportedMutationFailsClosed(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"delete","items":[]}}` + "\n")
	if _, err := (Trae{}).Read(bytes.NewReader(data)); err == nil {
		t.Fatal("unsupported mutation was silently accepted")
	}
}

func TestTraeHistoryKeepsUniqueSideRecords(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"message","id":"u1","role":"user","content":[{"type":"input_text","text":"hello"}]}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","id":"a1","role":"assistant","content":[{"type":"output_text","text":"unique response"}]}}` + "\n" +
		`{"type":"event_msg","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"a2","content":[{"type":"Text","text":"unique event"}]}}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || s.Messages[1].Parts[0].Text != "unique response" || s.Messages[2].Parts[0].Text != "unique event" {
		t.Fatalf("unique side records were dropped: %+v", s.Messages)
	}
}

func TestTraeDuplicateCallIDsOnlyCollapseExactReplay(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"function_call","call_id":"same","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"function_call","call_id":"same","name":"read","arguments":"{\"path\":\"a\"}"},{"type":"function_call","call_id":"same","name":"read","arguments":"{\"path\":\"b\"}"}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	firstArgs := string(s.Messages[0].Parts[0].ToolCall.Input)
	secondArgs := string(s.Messages[1].Parts[0].ToolCall.Input)
	var firstText, secondText string
	if json.Unmarshal(s.Messages[0].Parts[0].ToolCall.Input, &firstText) == nil {
		firstArgs = firstText
	}
	if json.Unmarshal(s.Messages[1].Parts[0].ToolCall.Input, &secondText) == nil {
		secondArgs = secondText
	}
	if len(s.Messages) != 2 || !strings.Contains(firstArgs, `"path":"a"`) || !strings.Contains(secondArgs, `"path":"b"`) {
		t.Fatalf("duplicate call replay handling changed distinct calls: %+v", s.Messages)
	}
}

func TestTraeReadToleratesTornFinalLine(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" + `{"type":"event_msg","payload":{"type":"user_message","message":"ok"}}` + "\n" + `{"type":"event_msg"`)
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil || len(s.Messages) != 1 {
		t.Fatalf("torn final line should be tolerated: err=%v session=%+v", err, s)
	}
	if !bytes.Equal(s.Original, data) {
		t.Fatal("torn source was not retained exactly")
	}
}

func TestTraeWriteReadSupportedSubset(t *testing.T) {
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174000",
		Workspace: "/work/project",
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "tool_call", ToolCall: &ToolCall{ID: "call-1", Name: "read", Input: json.RawMessage(`{"path":"x"}`), Status: "completed"}}}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "call-1", Content: "ok"}}}},
		},
	}
	var out bytes.Buffer
	if err := (Trae{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"source":"cli"`)) || !bytes.Contains(out.Bytes(), []byte(`"model_provider":"trae"`)) {
		t.Fatalf("writer omitted native Trae metadata: %s", out.Bytes())
	}
	got, err := (Trae{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if err := validateProjection(source, got); err != nil {
		t.Fatalf("written subset changed: %v", err)
	}
}

func TestTraeWriteRejectsInvalidSessionWithoutOutput(t *testing.T) {
	var out bytes.Buffer
	s := Session{NativeID: "not-a-uuid", Workspace: "/work", Messages: []Message{{Role: "user", Parts: []ContentBlock{{Type: "text", Text: "hello"}}}}}
	if err := (Trae{}).Write(&out, s); err == nil || out.Len() != 0 {
		t.Fatalf("invalid Trae id was accepted or partially written: err=%v output=%q", err, out.String())
	}
}

type traeShortWriter struct{}

func (traeShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestTraeWriteReportsShortWrite(t *testing.T) {
	s := Session{NativeID: "123e4567-e89b-12d3-a456-426614174000", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}}}}
	if err := (Trae{}).Write(traeShortWriter{}, s); err != io.ErrShortWrite {
		t.Fatalf("short writer error = %v, want %v", err, io.ErrShortWrite)
	}
}

func TestTraeRollbackUsesMutationTurnID(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","turn_id":"turn-old","items":[{"type":"message","id":"old","role":"user","content":"old"}]}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","turn_id":"turn-keep","items":[{"type":"message","id":"keep","role":"user","content":"keep"}]}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"rollback","target_turn_id":"turn-old","num_turns":1}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "keep" {
		t.Fatalf("rollback retained old turn: %+v", s.Messages)
	}
}
func TestTraeMultipleTurnRollbackFailsClosed(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" + `{"type":"history_mutation","payload":{"operation":"rollback","target_turn_id":"old","num_turns":2}}` + "\n")
	if _, err := (Trae{}).Read(bytes.NewReader(data)); err == nil {
		t.Fatal("unsupported multi-turn rollback was accepted")
	}
}

func TestTraeEventTurnIDParticipatesInRollback(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"turn-old","item":{"type":"AgentMessage","id":"old","content":[{"type":"Text","text":"old"}]}}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"user_message","turn_id":"turn-old","message":"old prompt"}}` + "\n" +
		`{"type":"event_msg","payload":{"type":"agent_message","turn_id":"turn-keep","message":"keep"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"rollback","target_turn_id":"turn-old","num_turns":1}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "keep" {
		t.Fatalf("event records ignored rollback turn ID: %+v", s.Messages)
	}
}

func TestTraeConversionReportsDeveloperHistoryItem(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"developer","role":"developer","content":[{"type":"input_text","text":"hidden context"}]},` +
		`{"type":"message","id":"user","role":"user","content":[{"type":"input_text","text":"real prompt"}]}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(joined, "history_mutation.item=message/developer=1") {
		t.Fatalf("developer item omission was not reported: %v", transfer.Limitations)
	}
}

func TestTraeContextEnvelopeWithUserTextIsNotDropped(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" + `{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","id":"user","role":"user","content":"<environment_context>example</environment_context> Please explain this."}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || !strings.Contains(s.Messages[0].Parts[0].Text, "Please explain this.") {
		t.Fatalf("user text was omitted: %+v", s.Messages)
	}
}

func TestTraeContextClassificationUsesSourceMetadata(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" + `{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"context","role":"user","content":[{"type":"input_text","text":"<system-reminder>one</system-reminder>"},{"type":"input_text","text":"<environment_context>two</environment_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["agents_md.instructions","environments.environment_context"]}},` +
		`{"type":"message","id":"actual","role":"user","content":"<environment_context>example</environment_context>","internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "<environment_context>example</environment_context>" {
		t.Fatalf("metadata classification failed: %+v", s.Messages)
	}
}

func TestTraeCompactionSummaryIsNotUserInput(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"summary","role":"user","content":[{"type":"input_text","text":"private compacted history"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["compaction.summary"]}},` +
		`{"type":"message","id":"prompt","role":"user","content":[{"type":"input_text","text":"continue from here"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}},` +
		`{"type":"message","id":"mixed","role":"user","content":[{"type":"input_text","text":"please explain this summary"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["compaction.summary","user.text"]}}]}}` + "\n")

	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Role != "user" || s.Messages[0].Parts[0].Text != "continue from here" || s.Messages[1].Parts[0].Text != "please explain this summary" {
		t.Fatalf("compaction summary was presented as user input: %+v", s.Messages)
	}
	if !bytes.Contains(s.Original, []byte("private compacted history")) {
		t.Fatal("source compaction summary was not retained for restoration")
	}

	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(limitations, "history_mutation.item=synthetic_context=1") {
		t.Fatalf("compaction summary omission was not reported: %v", transfer.Limitations)
	}
	if bytes.Contains(transfer.Target, []byte("private compacted history")) {
		t.Fatal("compaction summary leaked into target user messages")
	}
}

func TestTraeReasoningTextIsDroppedButReportedWithoutText(t *testing.T) {
	const secret = "private reasoning must not leak"
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"answer","role":"assistant","content":[{"type":"reasoning_text","text":"` + secret + `"},{"type":"output_text","text":"visible answer"}]},` +
		`{"type":"message","id":"source-answer","role":"assistant","content":[{"type":"source_reasoning","source":"reasoning","text":"another private thought"},{"type":"output_text","text":"second answer"}]}]}}` + "\n")

	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Parts[0].Text != "visible answer" || s.Messages[1].Parts[0].Text != "second answer" {
		t.Fatalf("reasoning content was exposed or visible text was lost: %+v", s.Messages)
	}
	if strings.Contains(s.Messages[0].Parts[0].Text+s.Messages[1].Parts[0].Text, secret) {
		t.Fatal("reasoning text leaked into the common transcript")
	}

	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	if !strings.Contains(limitations, "message.content=reasoning_text=1") || !strings.Contains(limitations, "message.content=source_reasoning=1") {
		t.Fatalf("reasoning omission was not reported by kind: %v", transfer.Limitations)
	}
	if strings.Contains(limitations, secret) || bytes.Contains(transfer.Target, []byte(secret)) {
		t.Fatal("reasoning text leaked into conversion output")
	}
}

func TestTraeTextSourceReasoningIsNotTranscriptText(t *testing.T) {
	const secret = "private source reasoning"
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"history_mutation","payload":{"operation":"append","items":[` +
		`{"type":"message","id":"answer","role":"assistant","content":[{"type":"text","source":"reasoning","text":"` + secret + `"},{"type":"output_text","text":"visible answer"}]}]}}` + "\n")

	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || len(s.Messages[0].Parts) != 1 || s.Messages[0].Parts[0].Text != "visible answer" {
		t.Fatalf("source reasoning was exposed or visible text was lost: %+v", s.Messages)
	}
	if strings.Contains(s.Messages[0].Parts[0].Text, secret) {
		t.Fatal("source reasoning leaked into the common transcript")
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(transfer.Target, []byte(secret)) {
		t.Fatal("target leaked source-reasoning text")
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "text(source=reasoning)") {
		t.Fatalf("dropped reasoning text was not reported: %v", transfer.Limitations)
	}
}

func TestTraeVisibleUserEventWithContextMarkersIsPreserved(t *testing.T) {
	data := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" + `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"Text","text":"<environment_context>user example</environment_context>"}]}}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Parts[0].Text != "<environment_context>user example</environment_context>" {
		t.Fatalf("visible user event was omitted: %+v", s.Messages)
	}
}

func TestTraeReadDropsReasoningFromObjectContentAndToolResultText(t *testing.T) {
	data := []byte(`{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/work"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:02Z","payload":{"type":"message","role":"assistant","content":{"type":"reasoning","text":"secret"}}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:03Z","payload":{"type":"function_call","call_id":"call-1","name":"run","arguments":"{}"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-01-01T00:00:04Z","payload":{"type":"function_call_output","call_id":"call-1","output":[{"type":"text","source":"reasoning","text":"SECRET"},{"type":"text","text":"ok"}]}}` + "\n")
	s, err := (Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 {
		t.Fatalf("reasoning-only object content was not dropped: %+v", s.Messages)
	}
	result := s.Messages[2].Parts[0].ToolResult
	if result.Content != "ok" || string(result.RawContent) != `[{"type":"text","text":"ok"}]` {
		t.Fatalf("reasoning leaked into tool result: content=%q raw=%s", result.Content, result.RawContent)
	}
}

// Trae tool outputs carrying reasoning items must not leak them through the
// structured RawContent that target writers serialize as JSON text, and the
// stripped reasoning must be reported like message-content reasoning.
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
		if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "message.content=reasoning_text") {
			t.Fatalf("%s: stripped tool-output reasoning was not reported: %v", target, transfer.Limitations)
		}
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

func TestConvertTraeReportsUnsupportedCompletedItemTypes(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174022","cwd":"/work/trae"}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","id":"command-1","command":"ls","status":"completed"}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"Reasoning","id":"reasoning-1","content":[{"type":"reasoning","text":"private"}]}}}
{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"AgentMessage","id":"answer-1","content":[{"type":"Text","text":"kept"}]}}}
`)
	source, err := (Trae{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transfer.Original, original) {
		t.Fatal("Trae conversion changed the source snapshot")
	}
	limitations := strings.Join(transfer.Limitations, "\n")
	for _, itemType := range []string{"CommandExecution", "Reasoning"} {
		if !strings.Contains(limitations, "event_msg.item_completed.item="+itemType+"=1") {
			t.Fatalf("unsupported completed item type %q was not reported: %v", itemType, transfer.Limitations)
		}
	}
}
