# Ferry

**Convert AI coding-agent session histories between tools — in Go.**

Ferry is a Go library that preserves coding-agent session files byte-for-byte and converts them between Claude Code, Codex, Gemini CLI, OpenCode, Trae CLI, Pi, Qoder CLI, DeepSeek Harness (dsh), Qwen Code, and GitHub Copilot CLI through a common Go representation. Use it to migrate sessions between CLIs, back up or archive agent histories, and build analytics or tooling on top of them.

[![CI](https://github.com/williamfzc/ferry/actions/workflows/ci.yml/badge.svg)](https://github.com/williamfzc/ferry/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/williamfzc/ferry.svg)](https://pkg.go.dev/github.com/williamfzc/ferry)
[![Go Report Card](https://goreportcard.com/badge/github.com/williamfzc/ferry)](https://goreportcard.com/report/github.com/williamfzc/ferry)
[![Codecov](https://codecov.io/gh/williamfzc/ferry/graph/badge.svg)](https://codecov.io/gh/williamfzc/ferry)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

## Features

- **Exact source capture** — `Session.Original` keeps every original byte, including unsupported records and line endings; `RestoreOriginal` reproduces the source file byte-for-byte (zstd-framed dsh logs restore as the compressed file).
- **Cross-provider conversion** — `Convert` writes a target projection, reads it back, and validates content, session identity, workspace, and timestamps. Anything the target cannot represent is listed in `Transfer.Limitations` instead of being dropped silently.
- **Discovery** — `Discover` scans every provider's default session directories, isolates per-file errors, and returns usable sessions newest-first.
- **Faithful messages** — ordered parts for text, tool calls, tool results, images, and unknown content; tool identity and outcomes are preserved across providers; numbers never pass through float64, so integers above 2^53 survive round trips.

## Installation

```bash
go get github.com/williamfzc/ferry
```

Ferry requires Go 1.22 or later. There is no binary to install; it is a library you import as:

```go
import "github.com/williamfzc/ferry/session"
```

## Quick start

Parse a session file, convert it to another provider's format, and write the projection:

```go
package main

import (
	"fmt"
	"log"
	"os"

	"github.com/williamfzc/ferry/session"
)

func main() {
	// Parse a Claude Code session file into the common representation.
	src, err := session.Parse("path/to/session.jsonl", "claude")
	if err != nil {
		log.Fatal(err)
	}

	// Convert it to a Codex rollout. The original bytes travel with the result.
	transfer, err := session.Convert(src, "codex")
	if err != nil {
		log.Fatal(err)
	}

	// Anything Codex cannot represent is reported, never dropped silently.
	for _, limitation := range transfer.Limitations {
		fmt.Println("limitation:", limitation)
	}

	if err := os.WriteFile("rollout.jsonl", transfer.Target, 0o644); err != nil {
		log.Fatal(err)
	}
}
```

`Reader` and `Writer` work on streams, and every provider implements both:

```go
var source session.Reader = session.OpenCode{}
s, err := source.Read(r)

var target session.Writer = session.Trae{}
err = target.Write(w, s)
```

To sweep every installed agent's default session directories:

```go
sessions, errs := session.Discover(os.Getenv("HOME"))
```

## Supported providers

All providers can be parsed, written, and used as conversion targets. The identifier is the value accepted by `Parse` and `Convert`.

| Provider | Identifier | On-disk format | Default location |
| --- | --- | --- | --- |
| Claude Code | `claude` | JSONL | `~/.claude/projects` |
| Codex CLI | `codex` | JSONL rollout | `~/.codex/sessions`, `~/.codex/archived_sessions` |
| Gemini CLI | `gemini` | JSONL history or legacy single-object session | `~/.gemini/tmp` |
| OpenCode | `opencode` | JSON document (`opencode export`) plus a line-oriented archive for storage sessions | `~/.local/share/opencode/storage/session` |
| Trae CLI | `trae` | JSONL, read recursively with symlinks skipped | `~/.trae/cli/sessions`, `~/.trae/cli/archived_sessions` |
| Pi | `pi` | JSONL | `~/.pi/agent/sessions` |
| Qoder CLI | `qoder` | JSONL, parent-linked record chain with an optional workspace sidecar | `~/.qoder/projects` |
| DeepSeek Harness (dsh) | `deepseek` | zstd-framed JSONL, newest log generation per directory | `~/.dsh/sessions` (`DSH_HOME`) |
| Qwen Code | `qwen` | JSONL chats plus sub-agent logs | `~/.qwen/projects` |
| Copilot CLI | `copilot` | JSONL event stream | `~/.copilot/session-state` (`COPILOT_HOME`) |

### Support matrix

Every provider implements `Reader` and `Writer`, works as a `Convert` target, and is covered by `Discover`. What differs is how each format was verified:

| Provider | Read | Write | Convert | Discover | Verified against |
| --- | :-: | :-: | :-: | :-: | --- |
| Claude Code | ✅ | ✅ | ✅ | ✅ | Native CLI smoke test |
| Codex CLI | ✅ | ✅ | ✅ | ✅ | Native CLI smoke test + real sessions |
| Gemini CLI | ✅ | ✅ | ✅ | ✅ | Native CLI smoke test |
| OpenCode | ✅ | ✅ | ✅ | ✅ | Native CLI smoke test (import/export) |
| Trae CLI | ✅ | ✅ | ✅ | ✅ | Codec + integration tests |
| Pi | ✅ | ✅ | ✅ | ✅ | Codec + integration tests |
| Qoder CLI | ⚠️ | ✅ | ✅ | ⚠️ | Codec + integration tests; current CLI layout not recognized yet |
| DeepSeek Harness (dsh) | ✅ | ✅ | ✅ | ✅ | Codec + integration tests (no native CLI check) |
| Qwen Code | ✅ | ✅ | ✅ | ✅ | Codec + integration tests (no native CLI check) |
| Copilot CLI | ✅ | ✅ | ✅ | ✅ | Codec + integration tests (no native CLI check) |

The one ⚠️ is Qoder: the codec follows the parent-linked chain layout of earlier Qoder CLI releases, while newer Qoder CLI releases write a Claude Code-shaped record layout that does not parse yet — see the [fidelity notes](docs/fidelity.md). Writing a Qoder session produces the earlier layout, so a current Qoder CLI may not read it back either. Cross-provider conversions are projections regardless of the marks: anything a target cannot represent is reported in `Transfer.Limitations`, never dropped silently.

## Documentation

- API reference: [pkg.go.dev/github.com/williamfzc/ferry/session](https://pkg.go.dev/github.com/williamfzc/ferry/session)
- [Fidelity rules and limits](docs/fidelity.md) — what conversions preserve, what they report as lost, verification levels, and the native CLI smoke checks

## Testing

```bash
make check   # gofmt, go vet, race-enabled tests
make cover   # tests with a coverage profile, prints the total
```

Optional native smoke scripts under [`scripts/`](scripts/) verify generated samples against the installed CLIs. They check discovery and parsing, not live resume:

```bash
sh scripts/native-claude-smoke-test.sh
sh scripts/native-codex-smoke-test.sh
sh scripts/native-gemini-smoke-test.sh
sh scripts/native-opencode-smoke-test.sh
sh scripts/native-pi-smoke-test.sh
sh scripts/native-trae-smoke-test.sh
```

CI runs the format, vet, and race tests on every push and pull request, and uploads the coverage profile to Codecov.

## Contributing

Pull requests are welcome. Please read [CONTRIBUTING.md](CONTRIBUTING.md) first — commit messages follow the [Angular convention](https://github.com/angular/angular/blob/main/CONTRIBUTING.md#commit).

## Acknowledgements

Ferry's format handling and session discovery are inspired by [agentsview](https://github.com/kenn-io/agentsview) and [deja-vu](https://github.com/vshulcz/deja-vu). The implementation is independently written.

## License

[Apache-2.0](LICENSE)
