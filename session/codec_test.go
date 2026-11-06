package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestCodecWritesAndReadsSummary(t *testing.T) {
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174000",
		Workspace: "/work",
		Messages: []Message{
			{Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "Earlier context"}}},
			{Role: "user", Parts: []ContentBlock{{Type: "text", Text: "Continue"}}},
		},
	}
	for _, tc := range []struct {
		name  string
		codec Codec
	}{
		{name: "claude", codec: Claude{}},
		{name: "codex", codec: Codex{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			if tc.name == "claude" {
				if err := tc.codec.Write(&out, source); err == nil || !strings.Contains(err.Error(), "summary cannot be represented") || out.Len() != 0 {
					t.Fatalf("Claude accepted an unverified summary mapping: err=%v output=%q", err, out.String())
				}
				return
			}
			if err := tc.codec.Write(&out, source); err != nil {
				t.Fatal(err)
			}
			got, err := tc.codec.Read(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Messages) != 2 || got.Messages[0].Role != "summary" || got.Messages[0].Parts[0].Text != "Earlier context" {
				t.Fatalf("summary was not preserved: %+v", got.Messages)
			}
			if tc.name == "codex" && !bytes.Contains(out.Bytes(), []byte(`"type":"compacted"`)) {
				t.Fatalf("Codex output did not use a compacted record: %s", out.Bytes())
			}
		})
	}
}

func TestExactJSONRejectsTrailingData(t *testing.T) {
	for _, raw := range []json.RawMessage{
		json.RawMessage(`{"n":1} {"n":2}`),
		json.RawMessage(`{"n":1} trailing garbage`),
	} {
		if _, err := exactJSON(raw); err == nil {
			t.Fatalf("exactJSON accepted trailing data: %q", raw)
		}
	}

	got, err := exactJSON(json.RawMessage(" {\"n\":9007199254740993} \n\t"))
	if err != nil {
		t.Fatalf("exactJSON rejected valid trailing whitespace: %v", err)
	}
	if string(got) != `{"n":9007199254740993}` {
		t.Fatalf("exactJSON changed the value: %s", got)
	}
}

func TestCodexWriterPreservesToolCallStatus(t *testing.T) {
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174000",
		Workspace: "/work",
		Messages: []Message{{Role: "assistant", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{
			Type: "tool_call", ToolCall: &ToolCall{ID: "call-1", Name: "read", Status: "completed", Input: json.RawMessage(`{}`)},
		}}}},
	}
	var out bytes.Buffer
	if err := (Codex{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"status":"completed"`)) {
		t.Fatalf("Codex output dropped tool status: %s", out.Bytes())
	}
	got, err := (Codex{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.Messages[0].Parts[0].ToolCall.Status != "completed" {
		t.Fatalf("Codex status did not survive read-back: %+v", got.Messages[0].Parts[0])
	}
}

func TestCodexSessionMetadataIncludesNativeFields(t *testing.T) {
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174000",
		Workspace: "/work",
		Messages:  []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}}},
	}
	var out bytes.Buffer
	if err := (Codex{}).Write(&out, source); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(bytes.NewReader(out.Bytes())).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Type      string          `json:"type"`
		Timestamp string          `json:"timestamp"`
		Payload   json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		ID         string `json:"id"`
		CWD        string `json:"cwd"`
		Timestamp  string `json:"timestamp"`
		Originator string `json:"originator"`
		CLIVersion string `json:"cli_version"`
	}
	if err := json.Unmarshal(record.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if record.Type != "session_meta" || record.Timestamp == "" || payload.Timestamp != record.Timestamp || payload.ID != source.NativeID || payload.CWD != source.Workspace || payload.Originator == "" || payload.CLIVersion == "" {
		t.Fatalf("incomplete Codex metadata: %s", line)
	}
}

func TestCodexMetadataDoesNotInventWorkspace(t *testing.T) {
	source := Session{
		NativeID: "123e4567-e89b-12d3-a456-426614174000",
		Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}}},
	}
	var out bytes.Buffer
	if err := (Codex{}).Write(&out, source); err == nil || !strings.Contains(err.Error(), "absolute path") || out.Len() != 0 {
		t.Fatalf("Codex accepted a session without an absolute workspace: err=%v output=%q", err, out.String())
	}
}

func TestCodecConvertsNativeImageBlocks(t *testing.T) {
	claudeImage := json.RawMessage(`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aGVsbG8="}}`)
	source := Session{
		NativeID:  "123e4567-e89b-12d3-a456-426614174001",
		Workspace: "/work",
		Messages:  []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "image", Raw: claudeImage}}}},
	}
	var codexOut bytes.Buffer
	if err := (Codex{}).Write(&codexOut, source); err != nil {
		t.Fatal(err)
	}
	codexSession, err := (Codex{}).Read(bytes.NewReader(codexOut.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(codexSession.Messages) != 1 || codexSession.Messages[0].Parts[0].Type != "input_image" {
		t.Fatalf("Codex image was not represented natively: %+v", codexSession.Messages)
	}
	if !bytes.Contains(codexOut.Bytes(), []byte(`"image_url":"data:image/png;base64,aGVsbG8="`)) {
		t.Fatalf("Codex image data was not preserved: %s", codexOut.Bytes())
	}

	var claudeOut bytes.Buffer
	if err := (Claude{}).Write(&claudeOut, codexSession); err != nil {
		t.Fatal(err)
	}
	claudeSession, err := (Claude{}).Read(bytes.NewReader(claudeOut.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var wantImage, gotImage any
	if err := json.Unmarshal(claudeImage, &wantImage); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(claudeSession.Messages[0].Parts[0].Raw, &gotImage); err != nil {
		t.Fatal(err)
	}
	if len(claudeSession.Messages) != 1 || claudeSession.Messages[0].Parts[0].Type != "image" || !reflect.DeepEqual(gotImage, wantImage) {
		t.Fatalf("Claude image did not round-trip: %+v", claudeSession.Messages)
	}
}

func TestCodecRejectsInvalidNativeImageWithoutOutput(t *testing.T) {
	source := Session{NativeID: "123e4567-e89b-12d3-a456-426614174002", Workspace: "/work", Messages: []Message{{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "image", Raw: json.RawMessage(`{"type":"image"}`)}}}}}
	for _, codec := range []Codec{Claude{}, Codex{}} {
		var out bytes.Buffer
		if err := codec.Write(&out, source); err == nil || !strings.Contains(err.Error(), `unsupported content "image"`) || out.Len() != 0 {
			t.Fatalf("invalid image was accepted or partially written: err=%v output=%q", err, out.String())
		}
	}
}

func TestCodecRejectsNonTextSummaryWithoutOutput(t *testing.T) {
	source := Session{NativeID: "123e4567-e89b-12d3-a456-426614174003", Workspace: "/work", Messages: []Message{{Role: "summary", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "image", Raw: json.RawMessage(`{"type":"image"}`)}}}}}
	for _, codec := range []Codec{Claude{}, Codex{}} {
		var out bytes.Buffer
		if err := codec.Write(&out, source); err == nil || !strings.Contains(err.Error(), `summary part 0: unsupported content "image"`) || out.Len() != 0 {
			t.Fatalf("non-text summary was accepted or partially written: err=%v output=%q", err, out.String())
		}
	}
}

func TestFerryUUIDUsesRFC4122NamespaceBytes(t *testing.T) {
	const want = "1204c617-e72b-5ba5-b224-59ded1728421"
	if got := ferryUUID("codex", "abc"); got != want {
		t.Fatalf("UUID v5 = %q; want %q", got, want)
	}
}

func TestClaudeOuterTypeOverridesNestedSummaryRole(t *testing.T) {
	in := `{"type":"assistant","sessionId":"s","message":{"role":"summary","content":[{"type":"text","text":"hi"}]}}` + "\n"
	s, err := Claude{}.Read(strings.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Role != "assistant" {
		t.Fatalf("nested role relabeled the assistant record: %+v", s.Messages)
	}
}

func TestFormatCountsSortsByName(t *testing.T) {
	if got := formatCounts(map[string]int{"b": 2, "a": 1, "c": 3}); got != "a=1, b=2, c=3" {
		t.Fatalf("formatCounts = %q", got)
	}
}

func TestCodexCustomToolCallJSONLookingInputStaysText(t *testing.T) {
	source := []byte(codexTestMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"custom_tool_call","call_id":"c1","name":"run","input":"{\"path\":\"x\"}","status":"completed"}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"custom_tool_call_output","call_id":"c1","output":"done"}}` + "\n")

	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	call := s.Messages[0].Parts[0].ToolCall
	var input string
	if err := json.Unmarshal(call.Input, &input); err != nil || input != `{"path":"x"}` {
		t.Fatalf("Codex custom input was not retained as text: %s", call.Input)
	}

	var out bytes.Buffer
	if err := (Codex{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out.Bytes(), []byte(`"type":"custom_tool_call"`)) {
		t.Fatalf("Codex changed custom tool call type: %s", out.Bytes())
	}
	if !bytes.Contains(out.Bytes(), []byte(`"status":"completed"`)) {
		t.Fatalf("Codex dropped custom tool call status: %s", out.Bytes())
	}
	if bytes.Contains(out.Bytes(), []byte(`"type":"function_call"`)) {
		t.Fatalf("Codex emitted a function call for custom text: %s", out.Bytes())
	}
	back, err := (Codex{}).Read(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip string
	if err := json.Unmarshal(back.Messages[0].Parts[0].ToolCall.Input, &roundTrip); err != nil || roundTrip != `{"path":"x"}` {
		t.Fatalf("Codex custom input changed after round trip: %s", back.Messages[0].Parts[0].ToolCall.Input)
	}

	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "1 custom tool calls with free-text input") {
		t.Fatalf("custom text wrapping was not reported: %v", transfer.Limitations)
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := target.Messages[0].Parts[0].ToolCall.Input
	var object map[string]string
	if err := json.Unmarshal(wrapped, &object); err != nil || object["input"] != `{"path":"x"}` {
		t.Fatalf("Claude did not wrap custom text: %s", wrapped)
	}

	for _, provider := range []string{"gemini", "opencode"} {
		transfer, err := Convert(s, provider)
		if err != nil {
			t.Fatalf("Convert to %s: %v", provider, err)
		}
		var target Session
		switch provider {
		case "gemini":
			target, err = (Gemini{}).Read(bytes.NewReader(transfer.Target))
		case "opencode":
			target, err = (OpenCode{}).Read(bytes.NewReader(transfer.Target))
		}
		if err != nil {
			t.Fatalf("read %s target: %v", provider, err)
		}
		var targetCall *ToolCall
		for _, message := range target.Messages {
			for _, part := range message.Parts {
				if part.Type == "tool_call" {
					targetCall = part.ToolCall
					break
				}
			}
			if targetCall != nil {
				break
			}
		}
		if targetCall == nil {
			t.Fatalf("%s target dropped custom tool call", provider)
		}
		var targetInput map[string]string
		if err := json.Unmarshal(targetCall.Input, &targetInput); err != nil || targetInput["input"] != `{"path":"x"}` {
			t.Fatalf("%s did not wrap custom text: %s", provider, targetCall.Input)
		}
	}
}

func TestClaudeKeepsEmptyTextAndUsesEnvelopeRole(t *testing.T) {
	data := []byte(`{"type":"user","sessionId":"id","message":{"content":""}}` + "\n")
	s, err := (Claude{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].Role != "user" || len(s.Messages[0].Parts) != 1 || s.Messages[0].Parts[0].Type != "text" || s.Messages[0].Parts[0].Text != "" {
		t.Fatalf("empty Claude message was not preserved: %+v", s.Messages)
	}
}

func TestClaudeUnknownRecordsRemainRestorable(t *testing.T) {
	data := []byte("{\"type\":\"system\",\"sessionId\":\"id\",\"cwd\":\"/tmp\",\"subtype\":\"init\"}\r\n")
	s, err := (Claude{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 0 || s.NativeID != "id" {
		t.Fatalf("unexpected unknown-only Claude session: %+v", s)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(restored.Bytes(), data) {
		t.Fatal("unknown Claude records were not preserved byte-for-byte")
	}
}

func TestReadersRejectOtherProviderRecords(t *testing.T) {
	claudeRecord := []byte(`{"type":"user","sessionId":"id","message":{"role":"user","content":"hello"}}` + "\n")
	if _, err := (Codex{}).Read(bytes.NewReader(claudeRecord)); err == nil {
		t.Fatal("Codex accepted a Claude record as a valid empty session")
	}

	codexRecord := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/tmp"}}` + "\n")
	if _, err := (Claude{}).Read(bytes.NewReader(codexRecord)); err == nil {
		t.Fatal("Claude accepted a Codex record as a valid empty session")
	}
}

func TestUnknownRecordAloneIsNotProviderEvidence(t *testing.T) {
	unknown := []byte(`{"type":"future_record","payload":{"kept":true}}` + "\n")
	for _, reader := range []Reader{Claude{}, Codex{}} {
		if _, err := reader.Read(bytes.NewReader(unknown)); err == nil {
			t.Fatal("reader accepted an unrelated future record without provider evidence")
		}
	}
}

func TestCodexCompactedAndEventsPreserveOrderWithoutDuplicateDialogue(t *testing.T) {
	data := "{" + `"type":"session_meta","payload":{"id":"id","cwd":"/tmp"}` + "}\n" +
		"{" + `"type":"compacted","timestamp":"1","payload":{"message":"summary"}` + "}\n" +
		"{" + `"type":"event_msg","timestamp":"2","payload":{"type":"user_message","message":"prompt"}` + "}\n" +
		"{" + `"type":"event_msg","timestamp":"3","payload":{"type":"agent_message","message":"stale event answer"}` + "}\n" +
		"{" + `"type":"response_item","timestamp":"4","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"prompt"}]}}` + "\n" +
		"{" + `"type":"event_msg","timestamp":"5","payload":{"type":"agent_message","message":"later event"}` + "}\n"
	s, err := (Codex{}).Read(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("unexpected messages: %+v", s.Messages)
	}
	if got := s.Messages[0].Parts[0].Text; got != "summary" || s.Messages[0].Role != "summary" {
		t.Fatalf("compaction moved or changed: %+v", s.Messages[0])
	}
	if got := s.Messages[1].Parts[0].Text; got != "stale event answer" || s.Messages[1].Role != "assistant" {
		t.Fatalf("unmatched event was discarded: %+v", s.Messages[1])
	}
	if got := s.Messages[2].Parts[0].Text; got != "prompt" || s.Messages[2].Role != "user" {
		t.Fatalf("canonical dialogue missing or reordered: %+v", s.Messages[2])
	}
	if got := s.Messages[3].Parts[0].Text; got != "later event" || s.Messages[3].Role != "assistant" {
		t.Fatalf("later event missing or reordered: %+v", s.Messages[3])
	}
}

func TestCodexEventBeforeToolCallIsNotDropped(t *testing.T) {
	data := "{" + `"type":"session_meta","payload":{"id":"id"}` + "}\n" +
		"{" + `"type":"event_msg","payload":{"type":"user_message","message":"prompt"}` + "}\n" +
		"{" + `"type":"response_item","payload":{"type":"function_call","call_id":"call","name":"shell","arguments":"{}"}}` + "\n"
	s, err := (Codex{}).Read(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 2 || s.Messages[0].Role != "user" || s.Messages[0].Parts[0].Text != "prompt" || s.Messages[1].Parts[0].ToolCall == nil {
		t.Fatalf("event before tool call was dropped: %+v", s.Messages)
	}
}

func TestCodexRepeatedTextInLaterTurnIsNotGloballyDeduplicated(t *testing.T) {
	data := "{" + `"type":"session_meta","payload":{"id":"id"}` + "}\n" +
		"{" + `"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"same"}]}}` + "\n" +
		"{" + `"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]}}` + "\n" +
		"{" + `"type":"event_msg","payload":{"type":"user_message","message":"same"}}` + "\n"
	s, err := (Codex{}).Read(bytes.NewReader([]byte(data)))
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 3 || s.Messages[2].Role != "user" || s.Messages[2].Parts[0].Text != "same" {
		t.Fatalf("legitimate repeated event turn was globally deduplicated: %+v", s.Messages)
	}
}

// Claude thinking blocks are skipped from the projection and reported.
func TestClaudeThinkingReported(t *testing.T) {
	source := []byte(`{"type":"user","sessionId":"123e4567-e89b-12d3-a456-426614174042","cwd":"/example","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}
{"type":"assistant","sessionId":"123e4567-e89b-12d3-a456-426614174042","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"reasoning here","signature":"sig"},{"type":"text","text":"answer"}]}}
`)
	s, err := (Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range s.Messages {
		for _, p := range m.Parts {
			if p.Type == "thinking" || p.Type == "unknown" {
				t.Fatalf("thinking block entered the model: %+v", p)
			}
		}
	}
	transfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "1 thinking blocks") {
		t.Fatalf("thinking not reported: %v", transfer.Limitations)
	}
}

// Codex delegation outputs without call_id cannot pair; they are reported.
func TestCodexDelegationOutputReported(t *testing.T) {
	source := []byte(codexTestMeta +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}}` + "\n" +
		`{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"function_call_output","id":"fco_1","name":"create_thread","namespace":"codex_app","output":"delegated"}}` + "\n")
	s, err := (Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(s, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "<delegation output without call_id>=1") {
		t.Fatalf("delegation output not reported: %v", transfer.Limitations)
	}
}

// Claude top-level summary records must join the messages instead of
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
	for _, limitation := range skippedClaudeLimitations(s) {
		if strings.Contains(limitation, "summary") {
			t.Fatalf("parsed summary reported as unrepresented: %s", limitation)
		}
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

const codexTestMeta = `{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/example"}}` + "\n"

// Developer and system role messages carry real instructions; they
// must be reported as unrepresented instead of vanishing.
func TestConvertReportsCodexRoleMessages(t *testing.T) {
	source := []byte(codexTestMeta +
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

// Compacted records can carry the replacement history with an empty
// summary; the unexpanded history must be reported.
func TestConvertReportsCompactedReplacementHistory(t *testing.T) {
	source := []byte(codexTestMeta +
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

// The Codex namespace qualifies MCP tool calls and must survive
// round trips; Claude receives the qualified single name.
func TestCodexNamespaceRoundTrip(t *testing.T) {
	source := []byte(codexTestMeta +
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
