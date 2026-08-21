# Contributing to wake

## Requirements

Only the Go toolchain (1.25 or newer). `make validate` pulls its own lint
tooling on first run — nothing else needs to be installed globally.

```sh
git clone https://github.com/SupermodularAI/agents-wake.git
cd agents-wake
make build
```

`make help` lists every target.

## Before opening a pull request

```sh
make validate
```

This runs the same gate CI runs: `fmt-check` → `vet` → `lint` → `test`. A
green local run means the lint/vet/test portion of CI will pass. Also run:
green local run means a green CI run. Also run:

```sh
make test-race
```

The release gate requires a race-clean suite.

## Pull requests

- Base branch is `main`.
- CI (lint, vet, tests, and a cross-platform build check) and one approving
  review are required before merge.
- Keep commits focused; squash-merge is used to land PRs, so intermediate
  commit history on your branch doesn't need to be pristine.
- Commit messages and PR titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `chore:`, ...).

## Code layout

- `cmd/wake/main.go` only calls into `internal/cli` — no logic there.
- `internal/cli` only parses flags and prints; everything else lives below it.
- Each subcommand lives in its own file under `internal/cli`.
- `internal/` is used by default. There is no `pkg/` — code intended for reuse
  outside this module is a design discussion, not a default choice.
- Colocate tests next to the code they cover (`foo.go` / `foo_test.go`).

## Reporting issues

Open a GitHub issue with steps to reproduce. Wake never writes raw transcript
content to logs or error messages, so redacted output from `wake doctor` is
safe to include.

## License

By contributing, you agree your contributions are licensed under the
project's [MIT license](LICENSE).
