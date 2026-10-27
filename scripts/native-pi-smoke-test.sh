#!/bin/sh

# Convert a small Codex transcript with Ferry, then let the installed Pi CLI
# read the generated native session and export it. Everything happens under a
# temporary home; --export only reads the session and never starts a turn.
set -eu

if ! command -v pi >/dev/null 2>&1; then
	printf '%s\n' 'pi CLI is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-pi-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

home=$probe_root/home
session_dir=$probe_root/pi-sessions
workspace=$probe_root/workspace
mkdir -p "$home" "$session_dir" "$workspace" "$probe_root/config" "$probe_root/data" "$probe_root/cache"

native_id=123e4567-e89b-12d3-a456-426614174000
session_file=$probe_root/ferry-pi-session.jsonl
html_file=$probe_root/ferry-pi-session.html
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
generator=$probe_root/generate.go

cat >"$generator" <<'EOF'
package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/williamfzc/ferry/session"
)

func verify(data []byte, wantID, wantWorkspace string) error {
	got, err := (session.Pi{}).Read(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("Pi read-back: %w", err)
	}
	if got.NativeID != wantID || got.Workspace != wantWorkspace {
		return fmt.Errorf("Pi identity changed: id=%q workspace=%q", got.NativeID, got.Workspace)
	}
	joined := ""
	for _, message := range got.Messages {
		for _, part := range message.Parts {
			joined += part.Text
			if part.ToolCall != nil {
				joined += part.ToolCall.Name
			}
			if part.ToolResult != nil {
				joined += part.ToolResult.Content
			}
		}
	}
	for _, needle := range []string{"pi ferry output ready", "read", "pi ferry tool result"} {
		if !strings.Contains(joined, needle) {
			return fmt.Errorf("Pi read-back is missing %q", needle)
		}
	}
	return nil
}

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: generate.go OUTPUT WORKSPACE NATIVE_ID")
		os.Exit(2)
	}
	output, workspace, nativeID := os.Args[1], os.Args[2], os.Args[3]
	source := []byte(fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":%q,"cwd":%q}}
{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ferry pi native smoke"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"pi ferry output ready"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:03Z","payload":{"type":"function_call","call_id":"pi-call-1","name":"read","arguments":"{\"path\":\"README.md\"}"}}
{"type":"response_item","timestamp":"2026-10-27T00:00:04Z","payload":{"type":"function_call_output","call_id":"pi-call-1","output":"pi ferry tool result"}}
`, nativeID, workspace))
	sessionIn, err := (session.Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		panic(fmt.Errorf("Codex fixture read: %w", err))
	}
	transfer, err := session.Convert(sessionIn, "pi")
	if err != nil {
		panic(fmt.Errorf("Ferry Codex to Pi conversion: %w", err))
	}
	if transfer.SourceType != "codex" || transfer.TargetType != "pi" || len(transfer.Target) == 0 {
		panic("unexpected Ferry conversion result")
	}
	if err := verify(transfer.Target, nativeID, workspace); err != nil {
		panic(err)
	}
	if err := os.WriteFile(output, transfer.Target, 0o600); err != nil {
		panic(err)
	}
	fmt.Printf("Ferry generated Pi session: %s\n", output)
}
EOF

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$session_file" "$workspace" "$native_id"
); then
	printf '%s\n' 'Ferry Pi writer failed to generate the session' >&2
	exit 1
fi

if ! result=$(
	HOME="$home" \
	XDG_CONFIG_HOME="$probe_root/config" \
	XDG_DATA_HOME="$probe_root/data" \
	XDG_CACHE_HOME="$probe_root/cache" \
	PI_OFFLINE=1 \
	TERM=xterm-256color \
	pi --offline --no-extensions --no-skills --session-dir "$session_dir" \
		--export "$session_file" "$html_file" 2>&1
); then
	printf '%s\n' "$result" >&2
	exit 1
fi

printf '%s\n' "$result"
if [ ! -s "$html_file" ]; then
	printf '%s\n' 'Pi did not create an export file' >&2
	exit 1
fi

printf '%s\n' 'native Pi smoke test passed'
