# Contributing to wake

## Requirements

Only the Rust toolchain (1.89 or newer, with `rustfmt` and `clippy`).

```sh
git clone https://github.com/SupermodularAI/agents-wake.git
cd agents-wake
cargo build --release
```

## Before opening a pull request

```sh
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
sh tests/launcher.sh
shellcheck install.sh package/bin/wake tests/launcher.sh
```

This is the same gate CI runs.

## Pull requests

- Base branch is `main`.
- CI and one approving review are required before merge. CI runs fmt, clippy, the tests on Linux
  and macOS, the perf budget and a check that the release binary is at most 2.5 MB. The OSS gates
  also scan the history for secrets (gitleaks), run `cargo audit` and check that the PR title is a
  Conventional Commit.
- Keep commits focused; squash-merge is used to land PRs, so intermediate
  commit history on your branch doesn't need to be pristine.
- Commit messages and PR titles follow [Conventional Commits](https://www.conventionalcommits.org/)
  (`feat:`, `fix:`, `chore:`, ...). Because PRs are squash-merged, the PR title
  is what lands on `main`.
- User-visible changes go under `## [Unreleased]` in [CHANGELOG.md](CHANGELOG.md).
  Internal refactors and test-only changes don't need an entry.

## Code layout

- `src/main.rs`: argument dispatch for `emit`, `check`, `install`, `uninstall` and `--version`; no logic.
- `src/lib.rs`: module declarations. Everything testable lives in the library.
- `src/event.rs`: the fixed event struct (`Invocation`, `Context`) and the string allowlist validators.
- `src/config.rs`: config resolution (env, then `telemetry.json` next to the binary, then `~/.config/wake/config.json`).
- `src/identity.rs`: user email lookup and the HMAC pseudonym.
- `src/repo.rs`: `origin` remote to `host/group/name`.
- `src/ids.rs`: deterministic trace and span ids.
- `src/otlp.rs`: invocations to an OTLP JSON trace request.
- `src/send.rs`: HTTP transport, the outbox and delivery.
- `src/state.rs`: per-session state, locking, garbage collection and state paths.
- `src/harness/mod.rs`: the contract a harness provides to derivation (`PrimitiveLookup`, `PrimitiveInfo`).
- `src/harness/claude_code/`: the Claude Code adapter (`transcript.rs` incremental JSONL reader,
  `inventory.rs` installed primitives, `derive.rs` transcript to invocations, `mod.rs` the hook enum and
  hook orchestration).
- `src/install.rs`: `wake install` / `uninstall` (the `settings.json` merge).
- `src/check.rs`: `wake check`.
- `tests/*.rs`: integration tests, with synthetic hand-written transcripts in `tests/fixtures/`. Never commit
  a real transcript. `tests/perf.rs` is ignored by default (`cargo test --release --test perf -- --ignored`).
- `tests/launcher.sh`: tests the APM launcher; run it with `sh tests/launcher.sh`.
- `package/`: the APM package template for organisations. No secrets, no binaries.
- `install.sh`: the release installer. CI runs `shellcheck` on it and on the launcher.

## Reporting issues

Open a GitHub issue with steps to reproduce. Wake never writes transcript
content to logs or error messages, so the output of `wake check` is
safe to include.

## License

By contributing, you agree your contributions are licensed under the
project's [MIT license](LICENSE).
