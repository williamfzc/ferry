package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const geminiIntegrationSession = `{"sessionId":"123e4567-e89b-12d3-a456-426614174010","projectHash":"deadbeef","startTime":"2026-01-01T00:00:00Z","lastUpdated":"2026-01-01T00:00:01Z","messages":[{"id":"u1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"hello"},{"id":"a1","type":"gemini","timestamp":"2026-01-01T00:00:01Z","content":[{"text":"ready"}]}]}`

func TestParseGeminiKeepsSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	original := []byte(geminiIntegrationSession + "\r\n")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Parse(path, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if s.SourceType != "gemini" || s.SourcePath != path || s.NativeID != "123e4567-e89b-12d3-a456-426614174010" || len(s.Messages) != 2 {
		t.Fatalf("unexpected parsed Gemini session: %+v", s)
	}
	if !bytes.Equal(s.Original, original) {
		t.Fatal("Gemini source snapshot was not preserved")
	}
	var restored bytes.Buffer
	if err := s.RestoreOriginal(&restored); err != nil || !bytes.Equal(restored.Bytes(), original) {
		t.Fatalf("Gemini source snapshot was not restorable: err=%v", err)
	}
}

func TestDiscoverFindsGeminiJSONSessions(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".gemini", "tmp", "deadbeef", "chats", "session.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(geminiIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 1 {
		t.Fatalf("Gemini discovery failed: sessions=%d errors=%v", len(sessions), errs)
	}
	if sessions[0].SourceType != "gemini" || sessions[0].NativeID != "123e4567-e89b-12d3-a456-426614174010" {
		t.Fatalf("unexpected discovered session: %+v", sessions[0])
	}
}

func TestDiscoverRestrictsGeminiChatsAndSkipsSymlinks(t *testing.T) {
	home := t.TempDir()
	valid := filepath.Join(home, ".gemini", "tmp", "deadbeef", "chats", "session.json")
	outsideDir := t.TempDir()
	outsideGemini := filepath.Join(outsideDir, "outside.json")
	if err := os.MkdirAll(filepath.Dir(valid), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(valid, []byte(geminiIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideGemini, []byte(geminiIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".gemini", "tmp", "deadbeef", "metadata.json"), []byte(geminiIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideGemini, filepath.Join(home, ".gemini", "tmp", "deadbeef", "chats", "linked.json")); err != nil {
		t.Fatal(err)
	}

	claudeOutside := filepath.Join(outsideDir, "outside.jsonl")
	if err := os.WriteFile(claudeOutside, []byte(`{"type":"user","sessionId":"outside","message":{"role":"user","content":"outside"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	claudeLink := filepath.Join(home, ".claude", "projects", "project", "linked.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudeLink), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(claudeOutside, claudeLink); err != nil {
		t.Fatal(err)
	}

	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 1 || sessions[0].SourceType != "gemini" {
		t.Fatalf("discovery followed an excluded path: sessions=%+v errors=%v", sessions, errs)
	}
}

func TestConvertGeminiToClaudeKeepsOriginal(t *testing.T) {
	original := []byte(geminiIntegrationSession)
	source, err := (Gemini{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transfer.Original, original) {
		t.Fatal("Gemini source snapshot changed during conversion")
	}
	target, err := (Claude{}).Read(bytes.NewReader(transfer.Target))
	if err != nil || len(target.Messages) != len(source.Messages) {
		t.Fatalf("Gemini projection is not readable: err=%v target=%+v", err, target)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "Gemini replay") {
		t.Fatalf("Gemini source limitation was not reported: %+v", transfer.Limitations)
	}
}

func TestConvertGeminiReportsUnsupportedMessageTypes(t *testing.T) {
	original := []byte(`{"sessionId":"123e4567-e89b-12d3-a456-426614174011","messages":[{"id":"system-1","type":"system","timestamp":"2026-01-01T00:00:00Z","content":"not portable"},{"id":"user-1","type":"user","timestamp":"2026-01-01T00:00:01Z","content":"keep"}]}`)
	source, err := (Gemini{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "system=1") {
		t.Fatalf("unsupported Gemini message type was not reported: %+v", transfer.Limitations)
	}
}

func TestConvertGeminiReportsUnknownTopLevelTypesWithoutID(t *testing.T) {
	original := []byte(
		`{"sessionId":"123e4567-e89b-12d3-a456-426614174012","projectHash":"deadbeef"}` + "\n" +
			`{"type":"future_state","payload":{"kept":true}}` + "\n" +
			`{"id":"user-1","type":"user","timestamp":"2026-01-01T00:00:00Z","content":"keep"}` + "\n")
	source, err := (Gemini{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "future_state=1") {
		t.Fatalf("unknown Gemini top-level type was not reported: %+v", transfer.Limitations)
	}
}

func TestConvertClaudeToGeminiReportsWorkspaceBoundary(t *testing.T) {
	original := []byte(`{"type":"user","sessionId":"claude-to-gemini","cwd":"/work/project","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hello"}}` + "\n")
	source, err := (Claude{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	target, err := (Gemini{}).Read(bytes.NewReader(transfer.Target))
	if err != nil || target.NativeID != source.NativeID || len(target.Messages) != 1 {
		t.Fatalf("Gemini projection is not readable: err=%v target=%+v", err, target)
	}
	if target.Workspace != "" {
		t.Fatalf("Gemini reader invented a workspace: %q", target.Workspace)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "project hash") {
		t.Fatalf("Gemini workspace limitation was not reported: %+v", transfer.Limitations)
	}
}

func TestConvertGeminiToCodexRejectsMissingWorkspace(t *testing.T) {
	source, err := (Gemini{}).Read(bytes.NewReader([]byte(geminiIntegrationSession)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(source, "codex"); err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Fatalf("Gemini conversion invented a workspace: %v", err)
	}
}

func TestConvertGeminiSameProviderKeepsRawSnapshot(t *testing.T) {
	original := []byte(geminiIntegrationSession + "\r\n")
	source, err := (Gemini{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "gemini")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(transfer.Target, original) || len(transfer.Limitations) != 0 {
		t.Fatalf("same-provider Gemini conversion changed source: %+v", transfer)
	}
}

func TestConvertGeminiRejectsEmptySession(t *testing.T) {
	source, err := (Gemini{}).Read(bytes.NewReader([]byte(`{"sessionId":"empty","messages":[]}`)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(source, "claude"); err == nil || !strings.Contains(err.Error(), "no supported messages") {
		t.Fatalf("empty Gemini conversion was accepted: %v", err)
	}
}
