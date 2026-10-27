// These tests check the OpenCode codec and storage assembly at their public
// boundary, mirroring the fixtures and schemas observed on CLI 1.18.32.
package session

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func readOpenCodeFixture(t *testing.T) Session {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "testdata", "opencode.json"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := (OpenCode{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOpenCodeReadExportDocument(t *testing.T) {
	s := readOpenCodeFixture(t)
	if s.SourceType != "opencode" || s.NativeID != "ses_example00000000000000000" || s.Workspace != "/example/project" {
		t.Fatalf("unexpected identity: %+v", s)
	}
	if len(s.Messages) != 4 {
		t.Fatalf("expected 4 messages (call and result split), got %d", len(s.Messages))
	}
	if s.Messages[0].Role != "user" || s.Messages[0].Parts[0].Text != "Fix the test" {
		t.Fatalf("unexpected first message: %+v", s.Messages[0])
	}
	second := s.Messages[1]
	if second.Role != "assistant" || len(second.Parts) != 2 {
		t.Fatalf("unexpected second message: %+v", second)
	}
	if second.Parts[0].Type != "text" || second.Parts[0].Text != "Checking" {
		t.Fatalf("progress markers or reasoning must not become parts: %+v", second.Parts)
	}
	call := second.Parts[1].ToolCall
	if second.Parts[1].Type != "tool_call" || call == nil || call.ID != "call-1" || call.Name != "bash" {
		t.Fatalf("unexpected tool call: %+v", second.Parts[1])
	}
	if !strings.Contains(string(call.Input), "go test") {
		t.Fatalf("tool input not preserved: %s", call.Input)
	}
	third := s.Messages[2]
	result := third.Parts[0].ToolResult
	if third.Role != "tool" || third.Parts[0].Type != "tool_result" || result == nil || result.ID != "call-1" || result.Content != "passed" {
		t.Fatalf("unexpected tool result message: %+v", third)
	}
	if s.Messages[0].Timestamp == "" || s.Messages[3].Parts[0].Text != "thanks" {
		t.Fatalf("timestamps or last message missing: %+v", s.Messages)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil || restored.Len() == 0 {
		t.Fatalf("restore failed: %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join("..", "testdata", "opencode.json")); !bytes.Equal(data, restored.Bytes()) {
		t.Fatal("export original bytes changed")
	}
}

func TestOpenCodeReadRejectsMessageFromAnotherSession(t *testing.T) {
	data := []byte(`{"info":{"id":"ses_one","directory":"/example","time":{"updated":1}},"messages":[{"info":{"id":"msg_other","sessionID":"ses_two","role":"user","time":{"created":1}},"parts":[{"type":"text","text":"hello"}]}]}`)
	if _, err := (OpenCode{}).Read(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "belongs to session") {
		t.Fatalf("foreign message accepted: %v", err)
	}
}

func TestOpenCodeWriteReadRoundTrip(t *testing.T) {
	s := readOpenCodeFixture(t)
	var out bytes.Buffer
	if err := (OpenCode{}).Write(&out, s); err != nil {
		t.Fatal(err)
	}
	got, err := (OpenCode{}).Read(&out)
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != s.NativeID || got.Workspace != s.Workspace {
		t.Fatalf("identity changed: %+v", got)
	}
	if err := validateProjection(s, got); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
}

func TestOpenCodeParallelToolCallsKeepIdentityAndOrder(t *testing.T) {
	source := Session{
		NativeID:  "ses_parallel000000000000000000",
		Workspace: "/example/project",
		Messages: []Message{
			{Role: "user", Timestamp: "2026-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "run both"}}},
			{Role: "assistant", Timestamp: "2026-01-01T00:00:01Z", Parts: []ContentBlock{
				{Type: "tool_call", ToolCall: &ToolCall{ID: "call-a", Name: "Read", Input: []byte(`{"path":"a"}`)}},
				{Type: "tool_call", ToolCall: &ToolCall{ID: "call-b", Name: "Read", Input: []byte(`{"path":"b"}`)}},
			}},
			{Role: "tool", Timestamp: "2026-01-01T00:00:02Z", Parts: []ContentBlock{
				{Type: "tool_result", ToolResult: &ToolResult{ID: "call-a", Content: "one"}},
				{Type: "tool_result", ToolResult: &ToolResult{ID: "call-b", Content: "two"}},
			}},
		},
	}
	var encoded bytes.Buffer
	if err := (OpenCode{}).Write(&encoded, source); err != nil {
		t.Fatal(err)
	}
	got, err := (OpenCode{}).Read(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if got.NativeID != source.NativeID || got.Workspace != source.Workspace {
		t.Fatalf("identity changed: got id=%q workspace=%q", got.NativeID, got.Workspace)
	}
	var sequence []string
	for _, message := range got.Messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				sequence = append(sequence, "call:"+part.ToolCall.ID)
			case "tool_result":
				sequence = append(sequence, "result:"+part.ToolResult.ID)
			}
		}
	}
	want := []string{"call:call-a", "result:call-a", "call:call-b", "result:call-b"}
	if !reflect.DeepEqual(sequence, want) {
		t.Fatalf("parallel call/result order changed: got %v want %v", sequence, want)
	}
}

func TestOpenCodeWriteRejectsLossySessions(t *testing.T) {
	base := readOpenCodeFixture(t)
	cases := []struct {
		name string
		mod  func(*Session)
	}{
		{"result without call", func(s *Session) {
			s.Messages = append(s.Messages, Message{Role: "tool", Timestamp: "2026-02-02T00:00:00Z", Parts: []ContentBlock{{Type: "tool_result", ToolResult: &ToolResult{ID: "missing", Content: "x"}}}})
		}},
		{"summary", func(s *Session) {
			s.Messages = append(s.Messages, Message{Role: "summary", Parts: []ContentBlock{{Type: "text", Text: "compact"}}})
		}},
		{"missing timestamp", func(s *Session) {
			s.Messages[0].Timestamp = ""
			s.Updated = time.Time{}
		}},
		{"unknown part", func(s *Session) {
			s.Messages[2].Parts = append(s.Messages[2].Parts, ContentBlock{Type: "image", Raw: []byte(`{"type":"image"}`)})
		}},
		{"missing workspace", func(s *Session) { s.Workspace = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base
			tc.mod(&s)
			var out bytes.Buffer
			if err := (OpenCode{}).Write(&out, s); err == nil || out.Len() != 0 {
				t.Fatalf("expected rejection without output, got err=%v output=%q", err, out.String())
			}
		})
	}
}

// writeOpenCodeStorage builds a minimal on-disk OpenCode store.
func writeOpenCodeStorage(t *testing.T, root string) string {
	t.Helper()
	sessionID := "ses_store0000000000000000000"
	projectDir := filepath.Join(root, "storage", "session", "projecthash")
	messageDir := filepath.Join(root, "storage", "message", sessionID)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "storage", "part", "msg_store0000000000000000b"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(projectDir, sessionID+".json"):                                                            `{"id":"` + sessionID + `","directory":"/example/store","time":{"created":1770000000000,"updated":1770000006000}}`,
		filepath.Join(messageDir, "msg_store0000000000000000a.json"):                                            `{"id":"msg_store0000000000000000a","sessionID":"` + sessionID + `","role":"user","time":{"created":1770000000000}}`,
		filepath.Join(messageDir, "msg_store0000000000000000b.json"):                                            `{"id":"msg_store0000000000000000b","sessionID":"` + sessionID + `","role":"assistant","time":{"created":1770000001000}}`,
		filepath.Join(root, "storage", "part", "msg_store0000000000000000b", "prt_store00000000000000001.json"): `{"id":"prt_store00000000000000001","messageID":"msg_store0000000000000000b","type":"text","text":"stored reply"}`,
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(projectDir, sessionID+".json")
}

func TestOpenCodeStorageAssemblyAndArchive(t *testing.T) {
	root := t.TempDir()
	sessionFile := writeOpenCodeStorage(t, root)
	s, err := Parse(sessionFile, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	if s.NativeID != "ses_store0000000000000000000" || s.Workspace != "/example/store" {
		t.Fatalf("unexpected identity: %+v", s)
	}
	if len(s.Messages) != 1 || s.Messages[0].Role != "assistant" || s.Messages[0].Parts[0].Text != "stored reply" {
		t.Fatalf("unexpected messages: %+v", s.Messages)
	}
	if !looksLikeOpenCodeArchive(s.Original) {
		t.Fatal("storage sessions must capture an archive original")
	}
	reread, err := (OpenCode{}).Read(bytes.NewReader(s.Original))
	if err != nil {
		t.Fatal(err)
	}
	if reread.NativeID != s.NativeID || reread.Workspace != s.Workspace || len(reread.Messages) != len(s.Messages) {
		t.Fatalf("archive read-back changed the session: %+v", reread)
	}
	if err := validateProjection(s, reread); err != nil {
		t.Fatalf("archive read-back changed content: %v", err)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil || !bytes.Equal(restored.Bytes(), s.Original) {
		t.Fatalf("archive restore failed: %v", err)
	}
	contents, err := openCodeReadArchive(s.Original)
	if err != nil {
		t.Fatalf("archive is not readable: %v", err)
	}
	found := false
	for _, msg := range contents.Messages {
		for _, raw := range msg.Parts {
			if bytes.Contains(raw, []byte("stored reply")) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("archive does not retain the original part bytes")
	}
}

func TestDiscoverOpenCodeStorage(t *testing.T) {
	home := t.TempDir()
	sessionFile := writeOpenCodeStorage(t, filepath.Join(home, ".local", "share", "opencode"))
	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 1 {
		t.Fatalf("got %d sessions, %d errors", len(sessions), len(errs))
	}
	if sessions[0].SourceType != "opencode" || sessions[0].SourcePath != sessionFile {
		t.Fatalf("unexpected discovery result: %+v", sessions[0])
	}
}

func TestConvertOpenCodeTargets(t *testing.T) {
	s := readOpenCodeFixture(t)
	for _, target := range []string{"claude", "codex", "opencode"} {
		t.Run(target, func(t *testing.T) {
			transfer, err := Convert(s, target)
			if err != nil {
				t.Fatal(err)
			}
			if transfer.SourceType != "opencode" || transfer.TargetType != target {
				t.Fatalf("unexpected transfer: %+v", transfer)
			}
			if !bytes.Equal(transfer.Original, s.Original) {
				t.Fatal("original snapshot missing")
			}
		})
	}
}

// A paired call and result give the Gemini writer everything it needs, so
// OpenCode sessions with finished tool calls convert to Gemini.
func TestConvertOpenCodeToGemini(t *testing.T) {
	s := readOpenCodeFixture(t)
	transfer, err := Convert(s, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transfer.Target), `"status":"success"`) {
		t.Fatalf("paired result did not derive a status:\n%s", transfer.Target)
	}
}

func TestConvertReportsOpenCodeSkips(t *testing.T) {
	s := readOpenCodeFixture(t)
	transfer, err := Convert(s, "codex")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(transfer.Limitations, "\n")
	for _, want := range []string{"reasoning=1", "step-finish=1", "step-start=1"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("limitations missing %q: %v", want, transfer.Limitations)
		}
	}
}

func TestConvertClaudeToOpenCode(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "testdata", "claude.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	source, err := (Claude{}).Read(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (OpenCode{}).Read(bytes.NewReader(transfer.Target))
	if err != nil {
		t.Fatal(err)
	}
	if target.NativeID != source.NativeID || target.Workspace != source.Workspace {
		t.Fatalf("identity changed: %+v", target)
	}
	if err := validateProjection(source, target); err != nil {
		t.Fatalf("projection changed: %v", err)
	}
}

func TestParseOpenCodeArchiveRetainsOriginal(t *testing.T) {
	root := t.TempDir()
	stored, err := Parse(writeOpenCodeStorage(t, root), "opencode")
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(root, "archive.jsonl")
	if err := os.WriteFile(archivePath, stored.Original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(archivePath, "opencode")
	if err != nil {
		t.Fatal(err)
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil {
		t.Fatal(err)
	}
	if s.SourcePath != archivePath || !bytes.Equal(restored.Bytes(), stored.Original) {
		t.Fatal("archive path or original bytes were not retained")
	}
	if _, err := Convert(s, "claude"); err != nil {
		t.Fatalf("archive cannot be converted: %v", err)
	}
}

func TestOpenCodeWriteReportsShortWrite(t *testing.T) {
	s := readOpenCodeFixture(t)
	if err := (OpenCode{}).Write(traeShortWriter{}, s); err != io.ErrShortWrite {
		t.Fatalf("short writer error = %v, want %v", err, io.ErrShortWrite)
	}
}
