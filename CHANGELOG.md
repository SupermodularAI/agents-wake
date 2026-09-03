# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
While Wake is pre-1.0, minor versions may carry breaking changes; they are
called out under Changed.

## [Unreleased]

### Added

- `SECURITY.md`, `CODE_OF_CONDUCT.md`, and this changelog.
- Secret, vulnerability, and commit-style gates in CI.

## [0.2.0] - 2026-08-28

Remote delivery. Wake can now ship its derived records to an OTLP/HTTP
JSON-compatible collector such as Langfuse. The capability ships in every
binary and stays off until it is configured — an unconfigured install sends
nothing.

### Added

- `wake remote` command surface: `set`, `on`, `off`, `flush`, and `status`.
  `wake remote set` prompts for the URL and both keys at a terminal, and reads
  the joined `public:secret` credential from standard input when scripted, so
  the secret never reaches the process table or shell history.
- OTLP/HTTP JSON delivery: one span per record, a trace per session, each
  invocation parented onto its session and named after the repository. Spans
  carry structure, timing, model, tokens, and outcome — never prompt or
  completion text. The emitted attribute key set is frozen by an equality
  assertion, so a new attribute cannot be added silently.
- `wake remote flush --dry-run` prints the exact payload the next flush would
  send, without sending it.
- Automatic flushing after a `wake ingest` scan once delivery is on, throttled
  by `remote.min_interval` (15 minutes by default, provisional), and reported
  when the interval holds one back.
- A `0600` endpoint and credential store, rejected on read if its mode is
  looser or if it sits in a directory another local user can write to.
- `WAKE_REMOTE_AUTHORIZATION` to override the stored credential, for CI or for
  anyone who prefers no secret on disk.
- Remote delivery state in `wake doctor`, reporting presence only; `wake remote
  status` is the only command that names the endpoint host.
- Per-primitive error rate in `wake report` and the dashboard.

### Changed

- Remote delivery moved out from behind a `remote` build tag and now ships in
  every build, off by default. CI asserts that a fresh release artefact has
  delivery disabled.
- `make validate` now covers the previously tagged surface.

### Fixed

- Timestamps outside `UnixNano`'s representable range are dropped rather than
  encoded.
- `wake remote set` no longer echoes the endpoint host.
- A truncated credential is refused rather than stored.

### Security

- Delivery ids derive from `event_id`, so a retried flush deduplicates at the
  receiver instead of double-counting.
- Nothing OTLP-shaped is persisted: the payload is computed at flush time, held
  for one POST, then discarded.
- The encoder fails closed — it re-validates every record on the way out, drops
  what it cannot represent, and returns the count so a caller can report
  blindness rather than silently reporting zero.

## [0.1.0] - 2026-08-20

First release. A local CLI for understanding which agent primitives a developer
uses, which ones fail, and which ones are never used, supporting Claude Code.

### Added

- `wake init` to activate collection for a project. Collection requires
  explicit, per-project consent and starts from the moment consent is given;
  importing existing history is a separate, explicit request.
- Claude Code transcript ingestion, deriving measurements without persisting
  prompts, tool arguments, code, repository paths, or repository labels.
- An idempotent local event store under the XDG state directory, with
  `WAKE_DIR` to relocate it and `wake ingest --rebuild` to recreate it from
  consented history.
- `wake report` for local activity reports, and a metrics dashboard bound to
  loopback only.
- Primitive inventory: which primitives are available, which are used, and
  which are never used.
- Configuration at `~/.config/wake/config.toml` with a known-key registry,
  validation on read and write, and a pure-Go TOML parser.
- Repository identity as a salted, per-machine HMAC, with the salt created once
  at `0600` and the readable project map kept local.
- `wake doctor` for environment diagnostics, reporting safe counters only, so
  its output is suitable for sharing in support requests.
- `wake update` and `wake update --check`, the only commands that reach the
  network without being asked to deliver.
- Official binaries for macOS and Linux on amd64 and arm64, plus `install.sh`.

[Unreleased]: https://github.com/SupermodularAI/agents-wake/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/SupermodularAI/agents-wake/releases/tag/v0.2.0
[0.1.0]: https://github.com/SupermodularAI/agents-wake/releases/tag/v0.1.0
