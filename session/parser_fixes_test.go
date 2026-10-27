package session

import (
	"bytes"
	"testing"
)

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
