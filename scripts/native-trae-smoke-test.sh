#!/bin/sh

# Convert a small Codex transcript with Ferry, then let the installed TRAE
# CLI inspect the generated native rollout. The blob migration command only
# reads/reformats a supplied file; it never starts a model turn. All TRAE
# state is redirected to a temporary home.
set -eu

if ! command -v trae-cli >/dev/null 2>&1; then
	printf '%s\n' 'trae-cli is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-trae-smoke.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

home=$probe_root/home
trae_home=$probe_root/trae-home
workspace=$probe_root/workspace
mkdir -p "$home" "$trae_home" "$workspace" "$probe_root/config" "$probe_root/data" "$probe_root/cache"

native_id=123e4567-e89b-12d3-a456-426614174000
rollout=$probe_root/ferry-trae-rollout.jsonl
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
	got, err := (session.Trae{}).Read(bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("Trae read-back: %w", err)
	}
	if got.NativeID != wantID || got.Workspace != wantWorkspace {
		return fmt.Errorf("Trae identity changed: id=%q workspace=%q", got.NativeID, got.Workspace)
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
	for _, needle := range []string{"trae ferry output ready", "read", "trae ferry tool result"} {
		if !strings.Contains(joined, needle) {
			return fmt.Errorf("Trae read-back is missing %q", needle)
		}
	}
	return nil
}

func main() {
	if len(os.Args) == 4 {
		output, workspace, nativeID := os.Args[1], os.Args[2], os.Args[3]
		source := []byte(fmt.Sprintf(`{"type":"session_meta","timestamp":"2026-10-27T00:00:00Z","payload":{"id":%q,"cwd":%q}}
{"type":"response_item","timestamp":"2026-10-27T00:00:01Z","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"ferry trae native smoke"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:02Z","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"trae ferry output ready"}]}}
{"type":"response_item","timestamp":"2026-10-27T00:00:03Z","payload":{"type":"function_call","call_id":"trae-call-1","name":"read","arguments":"{\"path\":\"README.md\"}"}}
{"type":"response_item","timestamp":"2026-10-27T00:00:04Z","payload":{"type":"function_call_output","call_id":"trae-call-1","output":"trae ferry tool result"}}
`, nativeID, workspace))
		sessionIn, err := (session.Codex{}).Read(bytes.NewReader(source))
		if err != nil {
			panic(fmt.Errorf("Codex fixture read: %w", err))
		}
		transfer, err := session.Convert(sessionIn, "trae")
		if err != nil {
			panic(fmt.Errorf("Ferry Codex to Trae conversion: %w", err))
		}
		if transfer.SourceType != "codex" || transfer.TargetType != "trae" || len(transfer.Target) == 0 {
			panic("unexpected Ferry conversion result")
		}
		if err := verify(transfer.Target, nativeID, workspace); err != nil {
			panic(err)
		}
		if err := os.WriteFile(output, transfer.Target, 0o600); err != nil {
			panic(err)
		}
		fmt.Printf("Ferry generated Trae rollout: %s\n", output)
		return
	}
	if len(os.Args) == 5 && os.Args[1] == "verify" {
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			panic(err)
		}
		if err := verify(data, os.Args[3], os.Args[4]); err != nil {
			panic(err)
		}
		fmt.Println("Ferry verified Trae rollout after CLI inspection")
		return
	}
	fmt.Fprintln(os.Stderr, "usage: generate.go OUTPUT WORKSPACE NATIVE_ID | verify ROLLOUT NATIVE_ID WORKSPACE")
	os.Exit(2)
}
EOF

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" "$rollout" "$workspace" "$native_id"
); then
	printf '%s\n' 'Ferry Trae writer failed to generate the rollout' >&2
	exit 1
fi

session_dir=$trae_home/sessions/2026/10/27
mkdir -p "$session_dir" "$home/.trae/cli/sessions/2026/10/27"
registered_rollout=$session_dir/rollout-2026-10-27T00-00-00-$native_id.jsonl
cp "$rollout" "$registered_rollout"
cp "$rollout" "$home/.trae/cli/sessions/2026/10/27/rollout-2026-10-27T00-00-00-$native_id.jsonl"
if ! native_result=$(
 HOME="$home" TRAE_HOME="$trae_home" TRAECLI_HOME="$trae_home" CODEX_HOME="$trae_home" XDG_CONFIG_HOME="$probe_root/config" \
 XDG_DATA_HOME="$probe_root/data" XDG_CACHE_HOME="$probe_root/cache" \
 trae-cli migrate-rollouts --json --verbose 2>&1
); then
 printf '%s\n' "$native_result" >&2
 exit 1
fi
printf '%s\n' "$native_result"
if ! printf '%s' "$native_result" | grep -Fq "$native_id" ||
   ! printf '%s' "$native_result" | grep -Fq '"status": "eligible"'; then
 printf '%s\n' 'TRAE CLI did not identify the generated session as eligible' >&2
 exit 1
fi

before_copy=$probe_root/before-rollout.jsonl
cp "$rollout" "$before_copy"
if ! result=$(
	HOME="$home" \
	TRAE_HOME="$trae_home" \
	XDG_CONFIG_HOME="$probe_root/config" \
	XDG_DATA_HOME="$probe_root/data" \
	XDG_CACHE_HOME="$probe_root/cache" \
	trae-cli migrate rollout-blobs "$rollout" 2>&1
); then
	printf '%s\n' "$result" >&2
	exit 1
fi

printf '%s\n' "$result"
if ! printf '%s\n' "$result" | grep -Fq 'No eligible inline rollout payloads found'; then
	printf '%s\n' 'TRAE CLI did not report a successful rollout inspection' >&2
	exit 1
fi
if ! cmp -s "$before_copy" "$rollout"; then
	printf '%s\n' 'TRAE CLI unexpectedly rewrote the text-only rollout' >&2
	exit 1
fi

if ! (
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run "$generator" verify "$rollout" "$native_id" "$workspace"
); then
	printf '%s\n' 'Ferry could not read the rollout after TRAE CLI inspection' >&2
	exit 1
fi

printf '%s\n' 'native Trae smoke test passed'
