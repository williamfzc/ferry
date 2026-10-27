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
