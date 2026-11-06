package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var allSources = []string{"claude", "codex", "gemini", "opencode", "trae", "pi", "qoder", "deepseek", "qwen", "copilot"}

func codecFor(t *testing.T, source string) Codec {
	t.Helper()
	switch source {
	case "claude":
		return Claude{}
	case "codex":
		return Codex{}
	case "gemini":
		return Gemini{}
	case "opencode":
		return OpenCode{}
	case "trae":
		return Trae{}
	case "pi":
		return Pi{}
	case "qoder":
		return Qoder{}
	case "deepseek":
		return DeepSeek{}
	case "qwen":
		return Qwen{}
	case "copilot":
		return Copilot{}
	}
	t.Fatalf("no codec for %q", source)
	return nil
}

// Every public Read must capture Original, so its output can be converted
// and restored without going through Parse.
func TestEveryCodecReadCapturesOriginal(t *testing.T) {
	in := Session{
		NativeID:  "0193a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b",
		Workspace: "/tmp/project",
		Messages: []Message{
			{Role: "user", Timestamp: "2025-01-01T00:00:00Z", Parts: []ContentBlock{{Type: "text", Text: "hello"}}},
			{Role: "assistant", Timestamp: "2025-01-01T00:00:01Z", Parts: []ContentBlock{{Type: "text", Text: "hi"}}},
		},
	}
	for _, source := range allSources {
		t.Run(source, func(t *testing.T) {
			codec := codecFor(t, source)
			var buf bytes.Buffer
			if err := codec.Write(&buf, in); err != nil {
				t.Fatal(err)
			}
			got, err := codec.Read(bytes.NewReader(buf.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Original) == 0 {
				t.Fatal("Read did not capture Original")
			}
			// Claude needs no workspace, which Gemini and Qoder streams lack.
			target := "claude"
			if source == "claude" {
				target = "pi"
			}
			if _, err := Convert(got, target); err != nil {
				t.Fatalf("Convert to %s: %v", target, err)
			}
		})
	}
}

func TestParseErrorsNameThePath(t *testing.T) {
	for _, source := range allSources {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.jsonl")
			if err := os.WriteFile(path, []byte("{bad\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := Parse(path, source)
			if err == nil {
				t.Fatal("expected a parse error")
			}
			if !strings.HasPrefix(err.Error(), path) || strings.Contains(err.Error(), ": :") {
				t.Fatalf("error %q does not name %s as a clean location", err, path)
			}
		})
	}
}
