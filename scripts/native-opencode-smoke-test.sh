#!/bin/sh

# Convert a synthetic Claude session (text, tool call, failed and successful
# tool results) through Ferry into the OpenCode export format, then validate
# it against the installed OpenCode CLI in an isolated data directory. The
# check never starts a model turn.
set -eu

if ! command -v opencode >/dev/null 2>&1; then
	printf '%s\n' 'opencode CLI is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-opencode-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

data_home=$probe_root/data-home
home=$probe_root/home
mkdir -p "$data_home" "$home"

session_id=ses_ferrysmoke000000000000000
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)
generator=$probe_root/generate.go
export_json=$probe_root/opencode-session.json

cat >"$generator" <<'EOF'
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/williamfzc/ferry/session"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: generate.go OUTPUT NATIVE_ID")
		os.Exit(2)
	}
	nativeID := os.Args[2]
	workspace := "/tmp/ferry-opencode-smoke"
	source := []byte(fmt.Sprintf(`{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:00Z","message":{"role":"user","content":"native smoke test"}}
{"type":"assistant","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:01Z","message":{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"call-ok","name":"Bash","input":{"command":"go test ./..."}}]}}
{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:02Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-ok","content":"passed"}]}}
{"type":"assistant","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:03Z","message":{"role":"assistant","content":[{"type":"text","text":"noted"},{"type":"tool_use","id":"call-bad","name":"Bash","input":{"command":"false"}}]}}
{"type":"user","sessionId":%q,"cwd":%q,"timestamp":"2026-10-27T00:00:04Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-bad","content":"exit status 1","is_error":true}]}}
`, nativeID, workspace, nativeID, workspace, nativeID, workspace, nativeID, workspace, nativeID, workspace))
	sessionIn, err := (session.Claude{}).Read(bytes.NewReader(source))
	if err != nil {
		panic(err)
	}
	transfer, err := session.Convert(sessionIn, "opencode")
	if err != nil {
		panic(err)
	}
	if transfer.SourceType != "claude" || transfer.TargetType != "opencode" || len(transfer.Target) == 0 {
		panic("unexpected Ferry conversion result")
	}
	if err := os.WriteFile(os.Args[1], transfer.Target, 0o600); err != nil {
		panic(err)
	}
}
EOF

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$export_json" "$session_id"
); then
	printf '%s\n' 'Ferry OpenCode writer failed to generate the session' >&2
	exit 1
fi

if ! result=$(
	XDG_DATA_HOME="$data_home" \
	HOME="$home" \
	PATH="$PATH" \
	opencode import "$export_json" 2>&1
); then
	printf '%s\n' "$result" >&2
	exit 1
fi

printf '%s\n' "$result"

case "$result" in
*"Imported session"*) ;;
*)
	printf '%s\n' 'native OpenCode import did not report the synthetic session as imported' >&2
	exit 1
	;;
esac

if ! listing=$(
	XDG_DATA_HOME="$data_home" \
	HOME="$home" \
	opencode session list 2>/dev/null
); then
	printf '%s\n' 'native OpenCode session list failed' >&2
	exit 1
fi

printf '%s\n' "$listing"

if ! printf '%s\n' "$listing" | grep -Fq "$session_id"; then
	printf '%s\n' 'native OpenCode session list did not include the imported session' >&2
	exit 1
fi

if ! exported=$(
	XDG_DATA_HOME="$data_home" \
	HOME="$home" \
	opencode export "$session_id" 2>/dev/null
); then
	printf '%s\n' 'native OpenCode export of the imported session failed' >&2
	exit 1
fi

printf '%s\n' "$exported" | head -5

for needle in 'native smoke test' 'call-ok' 'call-bad' '"error"'; do
	if ! printf '%s\n' "$exported" | grep -Fq "$needle"; then
		printf '%s\n' "native OpenCode export is missing $needle" >&2
		exit 1
	fi
done

printf '%s\n' 'native OpenCode smoke test passed'
