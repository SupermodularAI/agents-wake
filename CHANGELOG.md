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
- Invocations carry `duration_ms`. The field was declared on the record type and
  populated by nothing, so every exported span was zero-length. It is the
  request-to-result interval — from the `tool_use` instant to the `tool_result`
  instant — which includes scheduling and any wait for a human to approve a
  permission prompt, so a permission-gated call reads as slow. It is **not** tool
  execution time, and the harness's own `toolUseResult.durationMs` is deliberately
  not consulted or blended: one provenance, always. A call with no terminal result,
  and a pair whose result instant precedes its call, stay `nil` and are counted —
  never clamped to `0`, because `0` on the wire means a genuinely instant call.
- `doctor` splits `skipped transcripts` by reason, with transcripts from an
  unregistered worktree of a consented repository on their own line. That
  population was invisible inside one integer that summed three unrelated ones:
  over 1,422 transcripts on one machine it was 223, and nine days of collection
  the user had asked for were lost while the scan reported healthy. The other five
  lines are a directory that is not a repository, a repository nobody consented, a
  transcript predating its repository's consent instant, one nothing could
  classify, and one that held nothing terminal; the six sum to the count above
  them. A machine that has not scanned reads the breakdown as `not observed`
  rather than `0`, which is not the same answer. Classification registers nothing
  and consents nothing — the directories are counted, never collected from.
- `doctor` reports `pending subagent runs` — how many subagent runs the last scan
  could not resolve and carried to the next one rather than dropping. It is a separate
  line from `pending calls` and counts a different population: a tool call resolves
  when its result is written, a subagent run when its session closes. Without it a user
  whose runs are sitting unresolved in the carry reads a healthy scan and a confident
  zero — around 200 runs of one long session went unreported this way before the carry
  existed. The line counts every run the last scan still held unresolved, not the runs
  that scan newly deferred, and it never moves the integration state word: a carried
  run is not lost collection. It is the size of the carry's unresolved set and not a
  count of outstanding work, so read it that way: a run leaves the set only when a scan
  observes its session close, and nothing else evicts it, so a run whose transcripts
  the harness has pruned is counted from then on — including one that had already
  resolved and been written to the store before the pruning. The number does not fall
  back to zero by itself and grows over a machine's life.
- An MCP tool's invocation now records which server provided it. A server's tools
  are named `mcp__<server>__<tool>` by the harness, and the server segment is
  stored as the harness spells it, validated as a bounded token. Before this,
  every configured MCP server reported `invocations: 0` forever and read as
  unused however heavily it was used. A server that is configured but whose key
  does not match any observed prefix is reported as unmatched rather than
  rendered as a zero row.

### Changed

- **Breaking (CLI).** `wake uninstall` and `wake remove --purge` now ask before
  deleting anything. Both print the exact paths they will remove — `remove --purge`
  never printed them at all — and then wait for a yes on standard input; any other
  answer deletes nothing and says so. Where standard input is not a terminal, both
  refuse with a non-zero exit and change nothing unless `--yes` is given, so a
  script, cron job or CI step running either command today keeps working only once
  `--yes` is added. `--yes` skips the question, never the disclosure: the paths are
  still printed. When there is a question to answer, the paths are printed on the
  same stream the question is put on — standard error, where every prompt in Wake
  already goes — so redirecting standard output cannot leave you confirming a
  deletion whose paths you never saw; with `--yes` there is nothing to answer and
  they stay on standard output. `wake remove` without `--purge` is unchanged and
  deliberately not gated — it removes only Wake's hook entry, which `wake init`
  puts back.

- **Breaking (stored records and wire).** Two changes bump the record schema, which
  moves from 6 to 8.

  **Version 7** — the outcome `denied_policy` is now `denied_by_harness_rule`. The
  value records the harness's own permission rule refusing a tool, never a governance
  decision about an approved set — of the 159 such records on the first machine
  Wake was installed on, every one was a builtin Bash call. The old name invited
  a reader to take it for the latter. Anything grouping on the old string — a saved
  query or a dashboard panel — needs updating.

  **Version 8** — records carry the nullable `mcp_server` dimension described under
  Added.

  A store holding an earlier version is refused on
  read and re-derived from the harness's own history by the next scan you ask
  for (`wake ingest`) — a hook-fired scan reports the count and leaves the spool
  alone, since it collects inside each repository's boundary and could not put
  the records back (`wake doctor` shows the pending count under "records from an
  earlier schema version"). If you collect only through the hooks `wake init`
  installs, run `wake ingest` once after upgrading, or the older records
  stay unreadable and `wake report` shows only what was written since. The
  delivery watermark stamps the schema version and starts over on a bump, so
  nothing needs migrating by hand.

- **Breaking (`primitives.json`).** One primitive is now one row. The repository had
  entered both the aggregate's key and the snapshot's key, so a primitive used in
  several projects became several rows — on a real machine one skill showed as four
  rows of 2 / 2 / 1 / 1 where the answer to *how much do I use this* is 6. The
  repository now rides the row as a set instead of identifying it: counters are
  summed across projects and the error rate is recomputed over the merged
  population, never averaged from two rendered rates. `--unused` accordingly means
  **never used anywhere** — a primitive used in one project and not another has been
  used. The `PROJECT` cell still names a project where there is exactly one and
  shows a count where there are several; naming one would report it as the only
  project the primitive was used in. `primitiveFileVersion` goes 2 to 3 and an
  older snapshot is refused rather than migrated, so one refresh republishes it from
  the event spool. Nothing below the snapshot changes: the record contract, its
  `SchemaVersion` and the OTLP attribute set are untouched, and `wake.repo` still
  travels per invocation.

- `wake report` and the dashboard name the repository column **PROJECT** (`Project` in the
  dashboard; was `REPO`), and the docs now say what that value is: the project each invocation's
  own working directory resolved to, and for a linked worktree the repository it belongs to. Nothing about the value changes —
  no stored record, no identity, no delivered attribute; `wake.repo` and `wake.repo_label` carry
  exactly what they carried, and no re-ingest or `--rebuild` is needed. Only the reading changes:
  an agent driving work from one project into another checkout has that work counted under the
  driver. Measured on a real Claude Code corpus on 2026-09-08 (1,273 transcripts, 111,630 entries,
  19 consented repositories): of the 69 sessions that did resolvable file-changing work, 1 (1.4 %)
  did all of it in another project and 11 (15.9 %) touched more than one — a partial answer, not a
  wrong one.

### Fixed

- The `ERRORS` cell says what its percentage was computed over. It printed a bare
  `1 (100.0%)` beside a `CALLS` column reading 2, inviting a reader to bind the rate
  to the calls next to it; the real denominator is the calls that were rated at all.
  It now reads `1 of 1 rated (100.0%)`, from one renderer shared by the terminal
  report and the dashboard — the two had each kept a copy and the copies had
  diverged.

- A primitive none of whose calls carried an outcome renders as unrated rather than
  as `0`. Two correct facts — no failures seen, the calls happened — were forming a
  false sentence.

- A tool result that omits `is_error` is read as success for the families measured
  never to spell it. Before, an absent field meant *the source does not say* for
  every tool alike, so 61 % of Claude Code tool results carried no outcome and the
  null rate swamped every error rate the product renders. Across sixty transcripts,
  `is_error: false` is written by Bash alone — 929 occurrences; every other family
  omits on success and writes `true` on failure. The rule is a closed allowlist of
  the measured families, never an exception for Bash: a family nobody has measured
  keeps its absences unknown. No failure signal moves — denials, the interrupted
  flag and an explicit `true` keep their precedence.

- A subagent run whose own transcript ends in a failure is rated as a failure
  instead of carrying no outcome. Claude Code writes a structured
  `isApiErrorMessage` marker on 26 of 917 subagent transcripts, true in 26 of 26.
  Only the run's terminal entry counts. Success is still never derived — no side
  observes it — so absence stays unknown.

- A plugin skill no longer appears twice, once bare and once namespaced, with the
  bare row unreachable by any event — roughly 50 phantom rows in `--unused` on a
  real machine. A name folds only where it is provably one plugin's and nothing
  else's; every other case is refused rather than guessed, because folding wrongly
  merges two primitives' counters.

- A plugin-provided primitive takes its kind from the harness's own declaration,
  never from the directory it was discovered in. A plugin command that Claude Code
  lists and invokes as a skill was being counted as a command.

- `wake doctor` says which collection scope produced its `skipped transcripts`
  count. The same machine, the same transcripts and no consent change reported 1041
  skipped under the hook-fired scan and 143 under `wake ingest`; both numbers were
  right and nothing on screen said which question either answered. The counter
  file's own version goes 7 to 8 — a diagnostics file, unrelated to the record
  schema — so one scan's diagnostics are refused rather than explained wrongly.

- `wake init --help` no longer advertises a positional it refuses. The usage line
  read `wake init [path]`, but plain `init` takes no path — only `--global` does.
  The help now lists the four forms the unchanged validator accepts. Help surface
  only; no behaviour changed.

- A git worktree no longer splits one project across several rows in `wake report`,
  the dashboard and `primitives.json`. A worktree is still its own consented
  repository with its own identity and still needs its own `wake init`; what is new
  is that the entry records which repository it belongs to, and reports count its
  invocations under that repository. Delivery follows: `wake.repo_label` and
  `langfuse.trace.name` carry the parent repository's label, while `wake.repo` keeps
  the worktree's own hash, so grouping by hash still tells worktrees apart. A
  worktree consented before this release keeps its own row until you run `wake init`
  inside it again — nothing is rewritten on read. Spans already delivered keep the
  labels they were sent with; the correction is not retroactive, and no re-ingest or
  `--rebuild` is needed for local reports, because no repository hash changed.
  **Register the parent repository first:** a worktree discovered before its parent
  is consented registers with no relation and never gains one, so its invocations
  keep counting under the worktree.

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
