#!/bin/sh

# Prove native resume for a Ferry-converted session: convert a Codex session
# with Ferry, register the generated Trae rollout in an isolated session store,
# then ask the installed TRAE CLI to actually resume it with a tiny prompt and
# confirm the model answers. The resume step starts one real model turn on the
# logged-in account (costs tokens), unlike the other smoke scripts, which only
# inspect files. Requires trae-cli, go, and an existing trae-cli login.
#
# Usage: native-trae-resume-check.sh [CODEX_ROLLOUT]
#   CODEX_ROLLOUT  optional path to a real Codex rollout; default is a small
#                  built-in synthetic session. Real sessions exercise the full
#                  projection and cost the same single tiny turn.
set -eu

if [ $# -gt 1 ]; then
	printf '%s\n' 'usage: native-trae-resume-check.sh [CODEX_ROLLOUT]' >&2
	exit 2
fi
if ! command -v trae-cli >/dev/null 2>&1; then
	printf '%s\n' 'trae-cli is required' >&2
	exit 1
fi
if ! command -v go >/dev/null 2>&1; then
	printf '%s\n' 'go toolchain is required' >&2
	exit 1
fi

probe_root=$(mktemp -d "${TMPDIR:-/tmp}/ferry-trae-resume.XXXXXX")
trap 'rm -rf "$probe_root"' EXIT HUP INT TERM

home=$probe_root/home
trae_home=$probe_root/trae-home
workspace=$probe_root/workspace
mkdir -p "$home" "$trae_home" "$workspace" "$probe_root/config" "$probe_root/data" "$probe_root/cache"

rollout=$probe_root/ferry-trae-rollout.jsonl
script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
repo_root=$(CDPATH= cd -- "$script_dir/.." && pwd)

if ! go_result=$(
	cd "$repo_root" &&
	GOCACHE="$probe_root/go-cache" \
	GOPROXY=off \
	GOTOOLCHAIN=local \
	go run ./scripts/resume-check "${1:--}" "$rollout"
); then
	printf '%s\n' 'Ferry Codex -> Trae conversion failed' >&2
	exit 1
fi
printf '%s\n' "$go_result"
native_id=$(printf '%s\n' "$go_result" | sed -n 's/^RESUME_CHECK_NATIVE_ID=//p')
if [ -z "$native_id" ]; then
	printf '%s\n' 'conversion output missing RESUME_CHECK_NATIVE_ID' >&2
	exit 1
fi

# Trae keeps rollouts under TRAE_HOME/sessions/YYYY/MM/DD. The date here is the
# registration date, not the source timestamp; Trae discovers by directory.
day_dir=$trae_home/sessions/$(date +%Y/%m/%d)
mkdir -p "$day_dir"
registered=$day_dir/rollout-$(date +%Y-%m-%dT%H-%M-%S)-$native_id.jsonl
cp "$rollout" "$registered"

if ! native_result=$(
	HOME="$home" TRAE_HOME="$trae_home" TRAECLI_HOME="$trae_home" CODEX_HOME="$trae_home" \
	XDG_CONFIG_HOME="$probe_root/config" XDG_DATA_HOME="$probe_root/data" XDG_CACHE_HOME="$probe_root/cache" \
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

# The resume step needs the logged-in identity and config; copy them into the
# isolated home instead of touching the real ~/.trae. Hooks are copied too, so
# hook failures do not add noise to the run.
for f in auth.json installation_id traecli.toml; do
	if [ -f "$HOME/.trae/cli/$f" ]; then
		cp "$HOME/.trae/cli/$f" "$trae_home/$f"
	fi
done
if [ -d "$HOME/.trae/cli/hooks" ]; then
	cp -R "$HOME/.trae/cli/hooks" "$trae_home/hooks"
fi

printf '%s\n' '--- native resume ---'
if ! resume_result=$(
	HOME="$home" TRAE_HOME="$trae_home" TRAECLI_HOME="$trae_home" CODEX_HOME="$trae_home" \
	XDG_CONFIG_HOME="$probe_root/config" XDG_DATA_HOME="$probe_root/data" XDG_CACHE_HOME="$probe_root/cache" \
	trae-cli exec resume "$native_id" '只回复OK两个字，不要做任何其他事情' 2>&1
); then
	printf '%s\n' "$resume_result" >&2
	exit 1
fi
printf '%s\n' "$resume_result"
if ! printf '%s' "$resume_result" | grep -Fq "session id: $native_id"; then
	printf '%s\n' 'TRAE CLI did not resume the Ferry-generated session' >&2
	exit 1
fi
if ! printf '%s' "$resume_result" | grep -Eq '^OK$'; then
	printf '%s\n' 'TRAE CLI resumed the session but the model did not answer' >&2
	exit 1
fi

printf '%s\n' 'native Trae resume check passed: Ferry-converted Codex session resumed and answered'
