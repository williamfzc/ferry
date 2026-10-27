#!/bin/sh

# Convert a Ferry session with the Gemini writer, then have the installed
# Gemini CLI discover it from an isolated home. No model turn is started.
set -eu

if ! command -v gemini >/dev/null 2>&1; then
	printf '%s\n' 'gemini CLI is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-gemini-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

gemini_home=$probe_root/gemini-home
mkdir -p "$gemini_home/.gemini" "$probe_root/home"

native_id=123e4567-e89b-12d3-a456-426614174000
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
generator=$probe_root/generate.go

cat >"$generator" <<'EOF'
package main

import (
	"os"

	"github.com/williamfzc/ferry/session"
)

func main() {
	if len(os.Args) != 4 {
		panic("usage: generate.go OUTPUT WORKSPACE NATIVE_ID")
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()

	s := session.Session{
		NativeID:  os.Args[3],
		Workspace: os.Args[2],
		Messages: []session.Message{
			{Role: "user", Timestamp: "2026-10-27T00:00:00Z", Parts: []session.ContentBlock{{Type: "text", Text: "native Gemini smoke test"}}},
			{Role: "assistant", Timestamp: "2026-10-27T00:00:01Z", Parts: []session.ContentBlock{{Type: "text", Text: "ready"}}},
		},
	}
	if err := (session.Gemini{}).Write(f, s); err != nil {
		panic(err)
	}
}
EOF

run_gemini() {
	GEMINI_CLI_HOME="$gemini_home" HOME="$probe_root/home" GEMINI_API_KEY=dummy gemini "$@"
}

if ! run_gemini --list-sessions >/dev/null 2>&1; then
	printf '%s\n' 'Gemini CLI could not initialize its isolated session store' >&2
	exit 1
fi

project_root_file=$(find "$gemini_home/.gemini/tmp" -type f -name .project_root -print -quit)
if [ -z "$project_root_file" ]; then
	printf '%s\n' 'Gemini CLI did not create a project session directory' >&2
	exit 1
fi
chats_dir=$(dirname "$project_root_file")/chats
mkdir -p "$chats_dir"
rollout=$chats_dir/session-2026-10-27T00-00-${native_id%%-*}.json

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$rollout" "$repo_root" "$native_id"
); then
	printf '%s\n' 'Ferry Gemini writer failed to generate the rollout' >&2
	exit 1
fi

if ! result=$(run_gemini --list-sessions 2>&1); then
	printf '%s\n' "$result" >&2
	exit 1
fi

printf '%s\n' "$result"
if ! printf '%s\n' "$result" | grep -Fq "$native_id" ||
	! printf '%s\n' "$result" | grep -Fq 'native Gemini smoke test'; then
	printf '%s\n' 'Gemini CLI did not discover the Ferry-generated session' >&2
	exit 1
fi

printf '%s\n' 'native Gemini smoke test passed'
