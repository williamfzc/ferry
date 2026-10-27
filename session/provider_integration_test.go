package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const traeIntegrationSession = `{"type":"session_meta","timestamp":"2026-01-01T00:00:00Z","payload":{"id":"123e4567-e89b-12d3-a456-426614174020","cwd":"/work/trae","source":"cli","model_provider":"trae"}}
{"type":"history_mutation","timestamp":"2026-01-01T00:00:01Z","payload":{"operation":"append","items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}]}}
{"type":"world_state","timestamp":"2026-01-01T00:00:02Z","payload":{"branch":"main"}}
`

const piIntegrationSession = `{"type":"session","version":3,"id":"pi-session","timestamp":"2026-01-01T00:00:00Z","cwd":"/work/pi"}
{"type":"message","id":"u1","parentId":null,"timestamp":"2026-01-01T00:00:01Z","message":{"role":"user","content":"active"}}
{"type":"message","id":"a1","parentId":"u1","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"answer"}]}}
{"type":"message","id":"branch","parentId":"u1","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"inactive"}}
{"type":"model_change","id":"state","parentId":"branch","timestamp":"2026-01-01T00:00:04Z","message":{"model":"other"}}
`

func TestParseTraeAndPiKeepSourceSnapshots(t *testing.T) {
	for _, test := range []struct {
		name   string
		source string
		data   string
	}{
		{name: "trae", source: "trae", data: traeIntegrationSession},
		{name: "pi", source: "pi", data: piIntegrationSession},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "session.jsonl")
			original := []byte(test.data)
			if err := os.WriteFile(path, original, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := Parse(path, test.source)
			if err != nil {
				t.Fatal(err)
			}
			if s.SourceType != test.source || s.SourcePath != path || len(s.Messages) == 0 {
				t.Fatalf("unexpected parsed session: %+v", s)
			}
			if !bytes.Equal(s.Original, original) {
				t.Fatal("source snapshot changed")
			}
			var restored bytes.Buffer
			if err := s.RestoreOriginal(&restored); err != nil || !bytes.Equal(restored.Bytes(), original) {
				t.Fatalf("source snapshot was not restorable: %v", err)
			}
		})
	}
}

func TestDiscoverFindsTraeAndPiSessionsAndSkipsSymlinks(t *testing.T) {
	home := t.TempDir()
	traePath := filepath.Join(home, ".trae", "cli", "sessions", "nested", "trae.jsonl")
	traeArchivedPath := filepath.Join(home, ".trae", "cli", "archived_sessions", "trae-archived.jsonl")
	piPath := filepath.Join(home, ".pi", "agent", "sessions", "encoded", "pi.jsonl")
	for path, data := range map[string]string{traePath: traeIntegrationSession, traeArchivedPath: traeIntegrationSession, piPath: piIntegrationSession} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	outsideTrae := filepath.Join(outside, "trae.jsonl")
	outsidePi := filepath.Join(outside, "pi.jsonl")
	if err := os.WriteFile(outsideTrae, []byte(traeIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsidePi, []byte(piIntegrationSession), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideTrae, filepath.Join(home, ".trae", "cli", "sessions", "linked.jsonl")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsidePi, filepath.Join(home, ".pi", "agent", "sessions", "linked.jsonl")); err != nil {
		t.Fatal(err)
	}

	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 3 {
		t.Fatalf("unexpected discovery: sessions=%+v errors=%v", sessions, errs)
	}
	seen := map[string]bool{}
	for _, s := range sessions {
		seen[s.SourceType] = true
		if strings.Contains(s.SourcePath, "linked") {
			t.Fatalf("discovery followed symlink: %+v", s)
		}
	}
	if !seen["trae"] || !seen["pi"] {
		t.Fatalf("missing providers: %v", seen)
	}
}

func TestConvertTraeAndPiReportsUnsupportedRecords(t *testing.T) {
	trae, err := (Trae{}).Read(bytes.NewReader([]byte(traeIntegrationSession)))
	if err != nil {
		t.Fatal(err)
	}
	traeTransfer, err := Convert(trae, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(traeTransfer.Original, []byte(traeIntegrationSession)) {
		t.Fatal("Trae conversion changed the source snapshot")
	}
	if !strings.Contains(strings.Join(traeTransfer.Limitations, "\n"), "world_state=1") {
		t.Fatalf("Trae unsupported record was not reported: %+v", traeTransfer.Limitations)
	}
	if _, err := (Claude{}).Read(bytes.NewReader(traeTransfer.Target)); err != nil {
		t.Fatalf("Trae target is not readable: %v", err)
	}

	pi, err := (Pi{}).Read(bytes.NewReader([]byte(piIntegrationSession)))
	if err != nil {
		t.Fatal(err)
	}
	piTransfer, err := Convert(pi, "claude")
	if err != nil {
		t.Fatal(err)
	}
	limitations := strings.Join(piTransfer.Limitations, "\n")
	if !strings.Contains(limitations, "inactive_branch=") || !strings.Contains(limitations, "model_change=") {
		t.Fatalf("Pi unsupported records were not reported: %+v", piTransfer.Limitations)
	}
	if !bytes.Equal(piTransfer.Original, []byte(piIntegrationSession)) {
		t.Fatal("Pi conversion changed the source snapshot")
	}
}

func TestConvertTraeReportsDroppedReasoning(t *testing.T) {
	original := []byte(`{"type":"session_meta","payload":{"id":"123e4567-e89b-12d3-a456-426614174021","cwd":"/work/trae"}}
{"type":"history_mutation","payload":{"operation":"append","items":[{"type":"message","role":"user","content":[{"type":"thinking","text":"private"},{"type":"input_text","text":"keep"}]}]}}
`)
	source, err := (Trae{}).Read(bytes.NewReader(original))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(source, "claude")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "message.content=thinking=1") {
		t.Fatalf("dropped Trae reasoning was not reported: %+v", transfer.Limitations)
	}
}

func TestConvertToTraeAndPiUsesPublicTargets(t *testing.T) {
	trae, err := (Trae{}).Read(bytes.NewReader([]byte(traeIntegrationSession)))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Convert(trae, "pi"); err != nil {
		t.Fatalf("Trae to Pi conversion failed: %v", err)
	}

	pi, err := (Pi{}).Read(bytes.NewReader([]byte(piIntegrationSession)))
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := Convert(pi, "trae")
	if err != nil {
		t.Fatalf("Pi to Trae conversion failed: %v", err)
	}
	if _, err := (Trae{}).Read(bytes.NewReader(transfer.Target)); err != nil {
		t.Fatalf("Trae target read-back failed: %v", err)
	}
	if !strings.Contains(strings.Join(transfer.Limitations, "\n"), "Trae requires a UUID") {
		t.Fatalf("derived Trae identity was not reported: %+v", transfer.Limitations)
	}
}
