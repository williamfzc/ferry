# Contributing

Thanks for considering a contribution to Ferry.

## Development

Ferry needs Go 1.22 or later. The common entry points:

```bash
make check   # gofmt, go vet, race-enabled tests — must pass before pushing
make cover   # tests with a coverage profile, prints the total
```

## Commit messages

Commit messages follow the [Angular convention](https://github.com/angular/angular/blob/main/CONTRIBUTING.md#commit). Each commit message has the form:

```
<type>(<scope>)?: <subject>

<body>

<footer>
```

The **type** is one of:

| Type | Use for |
| --- | --- |
| `feat` | A new feature |
| `fix` | A bug fix |
| `docs` | Documentation-only changes |
| `style` | Formatting, missing semicolons, no production-code meaning |
| `refactor` | A change that neither fixes a bug nor adds a feature |
| `perf` | A change that improves performance |
| `test` | Adding missing or correcting existing tests |
| `build` | Build-system or dependency changes |
| `ci` | CI configuration changes |
| `chore` | Maintenance that touches none of the above |
| `revert` | Reverting a previous commit |

Rules for the **subject**:

- Use the imperative, present tense: "add", not "added" or "adds".
- Do not capitalize the first letter, and do not end with a period.
- Keep it to 72 characters or fewer. Do not repeat the project name (`Ferry: ...` is wrong; `feat: ...` is right).

The **body** is optional but encouraged for anything nontrivial: explain *what* and *why*, wrapped at 72 columns. Use `!` (for example `feat!:`) plus a `BREAKING CHANGE:` footer when a commit breaks an API.

Examples from this repository:

```
feat: add cross-agent session conversion library
docs: add contributing guide with Angular commit convention
ci: add GitHub Actions workflow with tests and coverage
```

To get the message skeleton in your editor, install the local template once:

```bash
git config commit.template .gitmessage
```

## Pull requests

- Title the PR with the same commit-message convention; squash-merges inherit it.
- Include or update tests for behavior changes; new providers need codec tests plus public `Parse`/`Discover`/`Convert` coverage.
- Run `make check` before pushing. CI runs the same checks on Go 1.22 and the latest stable release, and reports coverage to Codecov.
