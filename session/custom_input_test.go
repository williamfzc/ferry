package session

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const customInputCodexMeta = `{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174000","cwd":"/example"}}` + "\n"

func TestCodexCustomToolCallJSONLookingInputStaysText(t *testing.T) {
	source := []byte(customInputCodexMeta +
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
