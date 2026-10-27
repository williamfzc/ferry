#!/bin/sh

# Convert a synthetic Codex session through Ferry, then check that the
# installed Claude CLI discovers and parses the generated session file.
# Resume without a prompt exits before starting a model turn.
set -eu

if ! command -v claude >/dev/null 2>&1; then
	printf '%s\n' 'claude CLI is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-claude-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

config_dir=$probe_root/claude-config
workspace=$probe_root/workspace
mkdir -p "$config_dir" "$workspace"

native_id=123e4567-e89b-12d3-a456-426614174000
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
generator=$probe_root/generate.go

cat >"$generator" <<'EOF'
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/williamfzc/ferry/session"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: generate.go OUTPUT WORKSPACE NATIVE_ID")
		os.Exit(2)
	}
	output, workspace, nativeID := os.Args[1], os.Args[2], os.Args[3]
	source := []byte(fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":%q,"cwd":%q,"originator":"ferry-smoke"}}
{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"native smoke test"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ready"}]}}
`, nativeID, workspace))
	sessionIn, err := (session.Codex{}).Read(bytes.NewReader(source))
	if err != nil {
		panic(err)
	}
	transfer, err := session.Convert(sessionIn, "claude")
	if err != nil {
		panic(err)
	}
	if transfer.SourceType != "codex" || transfer.TargetType != "claude" || len(transfer.Target) == 0 {
		panic("unexpected Ferry conversion result")
	}
	if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
		panic(err)
	}
	if err := os.WriteFile(output, transfer.Target, 0o600); err != nil {
		panic(err)
	}
	fmt.Println(strings.Join([]string{output}, " "))
}
EOF

# Claude Code stores sessions under projects/<cwd with '/' replaced by '-' >.
project_dir=$config_dir/projects/$(printf '%s' "$workspace" | tr '/' '-')
session_file=$project_dir/$native_id.jsonl

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$session_file" "$workspace" "$native_id"
); then
	printf '%s\n' 'Ferry Claude writer failed to generate the session' >&2
	exit 1
fi

# Resume without a prompt always exits non-zero: with the not-found error and
# with the parsed-transcript error alike. Capture the output and judge by it.
status=0
result=$(
	cd "$workspace" &&
	CLAUDE_CONFIG_DIR="$config_dir" \
	TERM=xterm-256color \
	claude --resume "$native_id" </dev/null 2>&1
) || status=$?
if [ "$status" -ne 0 ] && [ -z "$result" ]; then
	printf 'claude resume exited %s without output\n' "$status" >&2
	exit 1
fi

printf '%s\n' "$result"

# A missing or unparseable session file makes the CLI report that no
# conversation was found; a parsed transcript produces a different message
# (for example a deferred-tool-marker complaint) and never this string.
case "$result" in
	*"No conversation found with session ID"*)
		printf '%s\n' 'native Claude resume did not discover the generated session' >&2
		exit 1
		;;
esac

printf '%s\n' 'native Claude smoke test passed'
