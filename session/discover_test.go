package session

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"testing"
)

// TestMain keeps Discover tests hermetic: the CLI home overrides it honors
// must not leak in from the developer's environment.
func TestMain(m *testing.M) {
	os.Unsetenv("DSH_HOME")
	os.Unsetenv("COPILOT_HOME")
	os.Exit(m.Run())
}

func writeTree(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for rel, data := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func discoveredRel(t *testing.T, home string) []string {
	t.Helper()
	sessions, errs := Discover(home)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	var got []string
	for _, s := range sessions {
		rel, err := filepath.Rel(home, s.SourcePath)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, s.SourceType+":"+filepath.ToSlash(rel))
	}
	sort.Strings(got)
	return got
}

func TestDiscoverQwenCopilotDeepSeekFiltering(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, map[string][]byte{
		".qwen/projects/p/chats/a.jsonl":             qwenFixture,
		".qwen/projects/p/subagents/s/agent-1.jsonl": qwenFixture,
		".qwen/projects/p/subagents/s/notes.jsonl":   qwenFixture,
		".qwen/projects/p/other.jsonl":               qwenFixture,
		".copilot/session-state/x/events.jsonl":      copilotFixture,
		".copilot/session-state/x/other.jsonl":       copilotFixture,
		".dsh/sessions/d/session.jsonl":              []byte("{bad"),
		".dsh/sessions/d/session.v2.jsonl":           deepSeekFixture,
	})
	want := []string{
		"copilot:.copilot/session-state/x/events.jsonl",
		"deepseek:.dsh/sessions/d/session.v2.jsonl",
		"qwen:.qwen/projects/p/chats/a.jsonl",
		"qwen:.qwen/projects/p/subagents/s/agent-1.jsonl",
	}
	if got := discoveredRel(t, home); !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v", got, want)
	}
}

func TestDiscoverHonorsCLIHomeOverrides(t *testing.T) {
	home := t.TempDir()
	writeTree(t, home, map[string][]byte{
		"dsh/sessions/d/session.jsonl":          deepSeekFixture,
		"cop/session-state/x/events.jsonl":      copilotFixture,
		".dsh/sessions/ignored/session.jsonl":   deepSeekFixture,
		".copilot/session-state/y/events.jsonl": copilotFixture,
	})
	// ~ resolves against the Discover argument, not the process HOME.
	t.Setenv("DSH_HOME", "~/dsh")
	t.Setenv("COPILOT_HOME", filepath.Join(home, "cop"))
	want := []string{
		"copilot:cop/session-state/x/events.jsonl",
		"deepseek:dsh/sessions/d/session.jsonl",
	}
	if got := discoveredRel(t, home); !slices.Equal(got, want) {
		t.Fatalf("discovered %v, want %v", got, want)
	}
}

func TestDiscoverFollowsSymlinkedProviderRoot(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()
	writeTree(t, real, map[string][]byte{"p/chats/a.jsonl": qwenFixture})
	writeTree(t, home, map[string][]byte{".dsh/sessions/d/session.jsonl": deepSeekFixture})
	if err := os.MkdirAll(filepath.Join(home, ".qwen"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(home, ".qwen", "projects")); err != nil {
		t.Skip(err)
	}
	dshReal := filepath.Join(home, ".dsh")
	if err := os.Rename(dshReal, filepath.Join(home, "dsh-real")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(home, "dsh-real"), dshReal); err != nil {
		t.Fatal(err)
	}
	sessions, errs := Discover(home)
	if len(errs) != 0 || len(sessions) != 2 {
		t.Fatalf("got %d sessions, errors %v; want 2 sessions", len(sessions), errs)
	}
}

func TestDiscoverReportsDeepSeekWalkErrors(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits not enforced")
	}
	home := t.TempDir()
	locked := filepath.Join(home, ".dsh", "sessions", "locked")
	writeTree(t, home, map[string][]byte{".dsh/sessions/locked/session.jsonl": deepSeekFixture})
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0700) })
	if _, errs := Discover(home); len(errs) != 1 {
		t.Fatalf("errors = %v, want the unreadable dsh directory", errs)
	}
}

func TestQwenAndCopilotOriginalIsByteExact(t *testing.T) {
	for source, fixture := range map[string][]byte{"qwen": qwenFixture, "copilot": copilotFixture} {
		s := parseProviderTemp(t, source, fixture)
		var restored bytes.Buffer
		if err := s.RestoreOriginal(&restored); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(s.Original, fixture) || !bytes.Equal(restored.Bytes(), fixture) {
			t.Fatalf("%s original bytes changed", source)
		}
	}
}
