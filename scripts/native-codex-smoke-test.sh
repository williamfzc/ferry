#!/bin/sh

# Convert a synthetic Claude session through Ferry, then validate the result
# against the installed Codex CLI. Migration is local and never starts a turn.
set -eu

if ! command -v codex >/dev/null 2>&1; then
	printf '%s\n' 'codex CLI is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-codex-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

codex_home=$probe_root/codex-home
mkdir -p "$codex_home/sessions/2026/10/27" "$probe_root/home"

native_id=123e4567-e89b-12d3-a456-426614174000
rollout=$codex_home/sessions/2026/10/27/rollout-2026-10-27T00-00-00-$native_id.jsonl
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
generator=$probe_root/generate.go

cat >"$generator" <<'EOF'
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/williamfzc/ferry/session"
)

func main() {
	if len(os.Args) != 4 {
		fmt.Fprintln(os.Stderr, "usage: generate.go OUTPUT WORKSPACE NATIVE_ID")
		os.Exit(2)
	}
	nativeID := os.Args[3]
	source := []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:01Z","message":{"role":"user","content":"native smoke test"}}
{"type":"assistant","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"ready"}]}}
`, nativeID, os.Args[2], nativeID, os.Args[2]))
	sessionIn, err := (session.Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		panic(err)
	}
	transfer, err := session.Convert(sessionIn, "codex")
	if err != nil {
		panic(err)
	}
	if transfer.SourceType != "claude" || transfer.TargetType != "codex" || len(transfer.Target) == 0 {
		panic("unexpected Ferry conversion result")
	}
	f, err := os.Create(os.Args[1])
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if _, err := f.Write(transfer.Target); err != nil {
		panic(err)
	}
}
EOF

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$rollout" "$repo_root" "$native_id"
); then
	printf '%s\n' 'Ferry Codex writer failed to generate the rollout' >&2
	exit 1
fi

if ! result=$(
	HOME="$probe_root/home" \
	CODEX_HOME="$codex_home" \
	TERM=xterm-256color \
	codex migrate-rollouts --json --verbose --apply 2>&1
); then
	printf '%s\n' "$result" >&2
	exit 1
fi

printf '%s\n' "$result"

if ! printf '%s\n' "$result" | grep -Fq "\"thread_id\": \"$native_id\"" ||
	! printf '%s\n' "$result" | grep -Fq '"status": "migrated"'; then
	printf '%s\n' 'native Codex migration did not report the synthetic session as migrated' >&2
	exit 1
fi

case "$result" in
	*'"bytes_processed": 0'*)
		printf '%s\n' 'native Codex migration reported zero processed bytes' >&2
		exit 1
		;;
esac

printf '%s\n' 'native Codex smoke test passed'
