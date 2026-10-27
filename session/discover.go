// This file discovers local sessions and isolates errors between files.
package session

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Discover isolates errors to individual files and returns them alongside usable sessions.
func Discover(home string) ([]Session, []error) {
	var sessions []Session
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
		{name: "copilot", dir: filepath.Join(home, ".copilot", "session-state"), exts: map[string]bool{".jsonl": true}},
	} {
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
			if src.name == "opencode" && !strings.HasPrefix(filepath.Base(path), "ses_") {
				return nil
			}
			if !src.exts[filepath.Ext(path)] {
				return nil
			}
			s, e := Parse(path, src.name)
			if e != nil {
				errs = append(errs, e)
			} else if len(s.Messages) > 0 {
				sessions = append(sessions, s)
			}
			return nil
		})
		if err != nil {
			errs = append(errs, err)
		}
	}
	for _, path := range discoverDeepSeekFiles(deepSeekSessionsRoot(home)) {
		s, e := Parse(path, "deepseek")
		if e != nil {
			errs = append(errs, e)
		} else if len(s.Messages) > 0 {
			sessions = append(sessions, s)
		}
	}
	sort.Slice(sessions, func(i, j int) bool { return sessions[i].Updated.After(sessions[j].Updated) })
	return sessions, errs
}

// deepSeekSessionsRoot resolves the dsh sessions directory, honoring the
// DSH_HOME override that the dsh CLI itself expands.
func deepSeekSessionsRoot(home string) string {
	if env := os.Getenv("DSH_HOME"); env != "" {
		if expanded, err := filepath.Abs(expandHome(env)); err == nil {
			return filepath.Join(expanded, "sessions")
		}
	}
	return filepath.Join(home, ".dsh", "sessions")
}

func expandHome(path string) string {
	if path == "~" {
		return os.Getenv("HOME")
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(os.Getenv("HOME"), path[2:])
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
