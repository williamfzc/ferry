// This file discovers local sessions and isolates errors between files.
package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Candidate is a session file found by Locate, not yet parsed.
type Candidate struct {
	Path   string
	Source string
}

// Discover isolates errors to individual files and returns them alongside usable sessions.
// Provider roots derive from home, except that DSH_HOME and COPILOT_HOME in the
// process environment override the dsh and Copilot roots, as the CLIs do.
// A provider root that is itself a symlink is followed; symlinks below it are not.
func Discover(home string) ([]Session, []error) {
	candidates, errs := Locate(home)
	var sessions []Session
	for _, c := range candidates {
		s, e := Parse(c.Path, c.Source)
		if e != nil {
			errs = append(errs, e)
		} else if len(s.Messages) > 0 {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	return sessions, errs
}

// Locate finds the session files Discover would parse without reading them,
// so callers can parse lazily or cache by file metadata. Root resolution
// follows the same rules as Discover.
func Locate(home string) ([]Candidate, []error) {
	var candidates []Candidate
	var errs []error
	for _, src := range []struct {
		name string
		dir  string
		exts map[string]bool
	}{
		{name: "claude", dir: filepath.Join(home, ".claude", "projects"), exts: map[string]bool{".jsonl": true}},
		{name: "codex", dir: filepath.Join(home, ".codex", "sessions"), exts: map[string]bool{".jsonl": true}},
		{name: "codex", dir: filepath.Join(home, ".codex", "archived_sessions"), exts: map[string]bool{".jsonl": true}},
		{name: "gemini", dir: filepath.Join(home, ".gemini", "tmp"), exts: map[string]bool{".json": true, ".jsonl": true}},
		{name: "opencode", dir: filepath.Join(home, ".local", "share", "opencode", "storage", "session"), exts: map[string]bool{".json": true}},
		{name: "trae", dir: filepath.Join(home, ".trae", "cli", "sessions"), exts: map[string]bool{".jsonl": true}},
		{name: "trae", dir: filepath.Join(home, ".trae", "cli", "archived_sessions"), exts: map[string]bool{".jsonl": true}},
		{name: "pi", dir: filepath.Join(home, ".pi", "agent", "sessions"), exts: map[string]bool{".jsonl": true}},
		{name: "qoder", dir: filepath.Join(home, ".qoder", "projects"), exts: map[string]bool{".jsonl": true}},
		{name: "qwen", dir: filepath.Join(home, ".qwen", "projects"), exts: map[string]bool{".jsonl": true}},
		{name: "copilot", dir: envRoot(home, "COPILOT_HOME", ".copilot", "session-state"), exts: map[string]bool{".jsonl": true}},
		{name: "doubaowork", dir: filepath.Join(home, "Library", "Application Support", "DoubaoWork", "Default", ".doubaowork", "agent_mode", "workspace", ".sessions"), exts: map[string]bool{".jsonl": true}},
	} {
		src.dir = resolveRootSymlink(src.dir)
		err := filepath.WalkDir(src.dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				errs = append(errs, err)
				return nil
			}
			if d.Type()&os.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			if src.name == "gemini" && !isGeminiChatFile(src.dir, path) {
				return nil
			}
			if src.name == "qwen" && !isQwenChatFile(src.dir, path) {
				return nil
			}
			if src.name == "copilot" && filepath.Base(path) != "events.jsonl" {
				return nil
			}
			if src.name == "doubaowork" && filepath.Base(path) != "trajectory.jsonl" {
				return nil
			}
			if src.name == "opencode" && !strings.HasPrefix(filepath.Base(path), "ses_") {
				return nil
			}
			if !src.exts[filepath.Ext(path)] {
				return nil
			}
			candidates = append(candidates, Candidate{Path: path, Source: src.name})
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	paths, walkErrs := discoverDeepSeekFiles(envRoot(home, "DSH_HOME", ".dsh", "sessions"))
	errs = append(errs, walkErrs...)
	for _, path := range paths {
		candidates = append(candidates, Candidate{Path: path, Source: "deepseek"})
	}
	return candidates, errs
}

// envRoot resolves a provider root, honoring a CLI home override such as
// DSH_HOME or COPILOT_HOME that replaces <home>/<defaultDir>.
func envRoot(home, env, defaultDir, sub string) string {
	if v := os.Getenv(env); v != "" {
		if expanded, err := filepath.Abs(expandHome(home, v)); err == nil {
			return filepath.Join(expanded, sub)
		}
	}
	return filepath.Join(home, defaultDir, sub)
}

// resolveRootSymlink follows a provider root that is itself a symlink, which
// WalkDir would otherwise report without descending. Other roots are returned
// unchanged so that session paths keep the caller's spelling.
func resolveRootSymlink(root string) string {
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		if r, err := filepath.EvalSymlinks(root); err == nil {
			return r
		}
	}
	return root
}

// expandHome resolves a leading ~ against home.
func expandHome(home, path string) string {
	if path == "~" {
		return home
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, path[2:])
	}
	return path
}

func isGeminiChatFile(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	return len(parts) == 3 && parts[0] != "" && parts[1] == "chats" && parts[2] != ""
}
