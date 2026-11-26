# Wake v2 (Claude Code adapter) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the Go v1 in `agents-wake` with a small Rust binary. Claude Code hooks call it. It derives one
event per skill, subagent, MCP tool or slash-command invocation from the current turn of the transcript, and sends
the events as OTLP/HTTP JSON spans to a configured endpoint (Langfuse first). The plan also covers the open-source
install path: `install.sh`, `wake install` / `wake uninstall`, and GitHub Releases.

**Architecture:** The binary is a single process per hook run. The hook payload arrives on stdin. The binary reads
the transcript incrementally from a per-session offset, applies the derivation rules (§5.5 of the spec), encodes the
events as OTLP spans, and POSTs them with a 2 s timeout. Failed sends go to a bounded outbox. The only state is
per-session JSON files and the outbox, under `~/.local/state/wake`. Claude Code is one implementation of a harness
module. Pi and Codex come in later plans.

**Tech Stack:** Rust 2021 (stable, ≥ 1.89 for `std::fs::File::try_lock`), `serde` + `serde_json`, `ureq` 2
(rustls), `hmac` + `sha2`. No CLI framework, no OTel SDK, no locking crate.

**Spec:** `docs/superpowers/specs/2026-11-26-wake-v2-design.md` (approved in commit `c826e1f`, amended with this
plan's review fixes: background subagents, `StopFailure`, MCP aggregation per turn, inventory format, stale-session
flush). Executors read the spec
alongside this plan. Section references (§n) point into it.

**Out of scope for this plan:**
- the Supermodular `smos-telemetry` APM package repo, and its CI;
- the Langfuse dashboards;
- the Pi and Codex adapters.

Each gets its own plan once this one ships a release.

## Global Constraints

- **Binary size:** release profile `opt-level = "z"`, `lto = true`, `codegen-units = 1`, `strip = true`,
  `panic = "abort"`. Target binary ≤ 2 MB on darwin-arm64.
- **Dependencies:** exactly `serde` (derive), `serde_json`, `ureq` (`default-features = false`,
  `features = ["tls"]`), `hmac`, `sha2`. Locking uses `std::fs::File::try_lock` (stable since Rust 1.89), and
  mtimes in tests use `std::fs::File::set_modified`. Set `rust-version = "1.89"` in `Cargo.toml`. Adding any other
  runtime crate needs a note in the PR saying why. Dev-dependencies are free.
- **`wake emit`:**
  - always exits 0;
  - never writes to stdout or stderr;
  - never panics outward. Wrap the body and swallow errors.
- **`wake emit` budget:** under 50 ms CPU and under 10 MB RSS for a one-turn slice of a 20 MB session transcript.
  Work grows with the turn, not with the session (§5.3).
- **What may leave the machine:** only the attributes listed in §5.6 and in Task 5's table. Never prompt text, tool
  inputs or outputs, Bash commands, file paths or local repo paths.
- **String allowlist:** every string attribute passes `event::is_token` (`^[A-Za-z0-9][A-Za-z0-9._:@+-]{0,127}$`
  with `_` in the second class, i.e. `[A-Za-z0-9._:@+_-]`: MCP names are `mcp__<server>__<tool>`),
  or `event::is_repo` for `wake.repo`, or `event::is_email` for `wake.user.email`. Failing values are dropped, never
  truncated or guessed.
- **Generic OTel variables:** `OTEL_EXPORTER_OTLP_*` are never read.
- **`WAKE_DISABLED`:** `1` or `true` makes `emit` return immediately, even when `allow_override=false`.
- **State dir:** `$XDG_STATE_HOME/wake`, else `~/.local/state/wake`.
- **Config file for the open-source path:** `~/.config/wake/config.json`, mode 0600.
- **Platforms:** macOS and Linux on arm64 and x86_64. No Windows.
- **Test fixtures:** synthetic, hand-written JSONL only. **Never commit real transcripts.** They contain client
  content.
- **Commits:** Conventional Commits, ending with
  `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Commit on branch `v2`, created from `v2-design`.

## Review Focus

These are failure modes the spec implies but does not spell out. Each one is pinned by a test in the task named.

1. **A partially written last line** (Claude Code is still flushing) is not consumed. The offset stops at the last
   complete line, and the next run picks the line up. *Task 7.*
2. **A session with no state file** (wake installed mid-session, state GC'd after 14 days, or `--resume` of an old
   session) never re-sends history. A missing state initialises its offset to the current file length. Only
   `SessionStart` with `source=startup` starts at 0. *Task 11.*
3. **An unreachable or hanging endpoint** makes `emit` finish within about 3 s. The outbox never exceeds 1,000
   entries or 7 days. *Task 6.*
4. **A `settings.json` that is not plain JSON** (comments, trailing commas, a symlink, a non-object) makes
   `wake install` refuse and change nothing. *Task 14.*
5. **Plugin-namespaced skill names** (`superpowers:brainstorming`) and MCP names with underscores
   (`mcp__playwright__click`) are kept. Names containing `/`, spaces or newlines are dropped. *Task 2.*
6. **Background subagents** (`run_in_background`) are the common case: 45 of 55 `Agent` results in a local sample
   were `status: "async_launched"`. Their result has no usage, type or duration, and their transcript is still being
   written at the parent's `Stop`. They stay open in the session state until their `<task-notification>` arrives.
   They are never reported as errors just for being async. *Tasks 10 and 12.*
7. **`<command-name>` inside other text** (skill bodies, pasted text, tool descriptions) is not a command. Only a
   user entry whose content *starts* with the tag, and that is not `isMeta`, counts. *Task 7.*
8. **Sessions that never get `SessionEnd`** (killed terminal, crash) still send their open invocations: GC closes
   and sends any open invocation idle for more than 30 minutes before it deletes a state file. *Tasks 11 and 12.*
9. **APM-installed hooks are not wake's to touch.** `wake install` / `uninstall` never match a hook whose command
   lives under `~/.claude/hooks/`. *Task 14.*

---

## File Structure

```
Cargo.toml
src/main.rs                     # arg dispatch: emit | check | install | uninstall
src/lib.rs                      # pub mod declarations; everything testable lives in the lib
src/event.rs                    # Invocation, Context, enums, allowlist validators
src/config.rs                   # Config, resolution (env > exe-dir telemetry.json > ~/.config/wake/config.json)
src/identity.rs                 # email lookup, HMAC pseudonym
src/repo.rs                     # origin remote → host/group/name
src/ids.rs                      # trace/span id derivation
src/otlp.rs                     # Invocation[] → ExportTraceServiceRequest JSON
src/send.rs                     # Transport trait, ureq transport, Outbox, deliver()
src/state.rs                    # SessionState, lock, GC, paths
src/harness/mod.rs              # Hook enum, PrimitiveLookup trait
src/harness/claude_code/transcript.rs   # incremental JSONL reader → Entry
src/harness/claude_code/inventory.rs    # installed primitives, PrimitiveLookup impl
src/harness/claude_code/derive.rs       # §5.5 rules → Vec<Invocation>
src/harness/claude_code/mod.rs          # run(hook, payload) orchestration
src/install.rs                  # wake install / uninstall (settings.json merge)
src/check.rs                    # wake check
tests/fixtures/claude_code/*.jsonl      # synthetic transcripts
tests/*.rs                      # integration tests named per task
install.sh
package/                        # APM package template for organisations (no secrets)
.github/workflows/ci.yml        # replaced: cargo test + size check
.github/workflows/release.yml   # replaced: cross-compile + GitHub Release
README.md  docs/langfuse-setup.md
```

---

### Task 1: Retire v1 and scaffold the crate

**Files:**
- Delete: `cmd/`, `internal/`, `e2e/`, `testdata/`, `go.mod`, `go.sum`, `go.tools.mod`, `go.tools.sum`, `Makefile`,
  `.golangci.yml`, `.goreleaser.yaml`, v1's `install.sh` (Task 15 writes the v2 one), and `.github/workflows/ci.yml`
  and `release.yml`
- Modify: `.github/workflows/oss-gates.yml`. Keep the `secrets` (gitleaks) and `commit-style` jobs. Replace the
  Go `vulnerabilities` job (`setup-go` + `govulncheck`) with `cargo audit` (installed with `cargo install --locked
  cargo-audit`).
- Keep: `LICENSE`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md` (update the build commands), `SECURITY.md`, `CHANGELOG.md`,
  `.gitleaks.toml`, `docs/`
- Create: `Cargo.toml`, `src/main.rs`, `src/lib.rs`, `.github/workflows/ci.yml`, `tests/cli_smoke.rs`

**Interfaces:**
- Produces: binary `wake`; `lib` crate `wake` with empty modules from the File Structure.
- Produces: `fn main()`, which dispatches on `args[1]`:
  - `emit <hook>`: calls `wake::harness::claude_code::run_hook`, a stub returning `()` until Task 12;
  - `check`, `install`, `uninstall`: stubs until Tasks 13 and 14;
  - an unknown subcommand prints usage to stderr and exits 2. The exception is `emit`, which always exits 0.

- [ ] **Step 1:** Tag v1 locally: `git tag v1-final $(git merge-base v2-design main)`. Do **not** push the tag; the
  user pushes it.
- [ ] **Step 2:** Create branch `v2` from `v2-design` and delete the v1 tree listed above.
- [ ] **Step 3: Write the failing test** `tests/cli_smoke.rs`:
  - `emit_unknown_hook_exits_zero_silently`: spawn `wake emit nonsense` with stdin `"{}"`, and assert exit 0, empty
    stdout and empty stderr;
  - `emit_garbage_stdin_exits_zero`: send stdin `"not json"` and expect the same;
  - `unknown_subcommand_exits_2`: run `wake frobnicate` and assert exit code 2.
- [ ] **Step 4:** Run `cargo test --test cli_smoke`. Expected: it fails to compile, or the tests fail.
- [ ] **Step 5:** Write `Cargo.toml` with the dependencies and release profile from Global Constraints, plus
  `[dev-dependencies] tempfile = "3"`. Then write `main.rs` and `lib.rs` as described.
- [ ] **Step 6:** Run `cargo test` and expect PASS. Then run `cargo build --release && ls -l target/release/wake`.
  Expected: under 2 MB.
- [ ] **Step 7:** Replace `.github/workflows/ci.yml` with `cargo fmt --check`, `cargo clippy -- -D warnings`,
  `cargo test` on ubuntu-latest and macos-latest, and a step that fails if the release binary is over 2 MB.
- [ ] **Step 8: Commit** `chore(v2): retire the Go v1 tree and scaffold the Rust crate`.

### Task 2: Event model and allowlist

**Files:**
- Create: `src/event.rs`
- Test: `tests/event.rs`

**Interfaces:**
- Produces:
  ```rust
  pub enum Kind { Skill, Subagent, McpTool, Command, Inventory }          // as_str: "skill","subagent","mcp_tool","command","inventory"
  pub enum Invoker { User, Model, Nested }                                 // "user","model","nested"
  pub enum Outcome { Ok, Error, Interrupted }                              // "ok","error","interrupted"
  #[derive(Default, Clone, Copy, PartialEq, Debug, Serialize, Deserialize)]
  pub struct Tokens { pub input: Option<u64>, pub output: Option<u64>, pub cache_read: Option<u64>, pub cache_creation: Option<u64> }
  impl Tokens { pub fn add(&mut self, other: &Tokens) }                    // None + Some(x) = Some(x)
  pub struct McpAgg { pub server: String, pub calls: u64, pub errors: u64, pub total_duration_ms: u64, pub max_duration_ms: u64 }
  pub struct Invocation {
      pub span_id: [u8; 8], pub parent_span_id: Option<[u8; 8]>,
      pub start_ns: u64, pub end_ns: u64,
      pub kind: Kind, pub name: String,
      pub version: Option<String>, pub plugin: Option<String>, pub marketplace: Option<String>,
      pub context_tokens: Option<u64>,
      pub invoker: Option<Invoker>, pub outcome: Option<Outcome>,
      pub tool_calls: Option<u64>, pub tool_errors: Option<u64>, pub tools: Vec<(String, u64)>,
      pub mcp: Option<McpAgg>, pub model: Option<String>, pub tokens: Tokens,
      pub inventory: Vec<String>,                                          // only for Kind::Inventory
  }
  pub enum UserId { Email(String), Hash(String), Unknown }
  pub struct Context { pub harness: &'static str, pub harness_version: Option<String>, pub session_id: String, pub user: UserId, pub repo: Option<String> }
  pub fn is_token(s: &str) -> bool;
  pub fn is_repo(s: &str) -> bool;   // 2..=8 '/'-separated segments, each is_token, total ≤ 255, no ".."
  pub fn is_email(s: &str) -> bool;  // one '@', local and domain non-empty, chars [A-Za-z0-9._%+-] / [A-Za-z0-9.-], ≤ 254
  impl Invocation { pub fn sanitized(self) -> Option<Invocation> }  // None if name fails is_token; drops failing optional strings and tools entries
  impl Context { pub fn sanitized(self) -> Context }
  ```

- [ ] **Step 1: Write the failing tests** in `tests/event.rs`:
  - `token_accepts_plugin_namespace`: `is_token("superpowers:brainstorming")` is true.
  - `token_accepts_underscores`: `is_token("mcp__playwright__click")` and `is_token("ai103_labs")` are true.
  - `token_rejects_paths_spaces_newlines`: false for each of `"/Users/x/skill"`, `"a b"`, `"a\nb"`, `""`, and a
    129-character string.
  - `repo_accepts_host_group_name`: `is_repo("gitlab.com/supermodularai/core/agents-wake")` is true.
  - `repo_rejects`: false for `"agents-wake"` (one segment), `"/Users/x/repo"` (empty first segment),
    `"gitlab.com/../x"`.
  - `email_rules`: `is_email("jose.morais@supermodular.ai")` is true. `"no-at"` and `"a@b@c"` are false.
  - `sanitized_drops_invalid_name`: an invocation named `"rm -rf /"` returns `None`.
  - `sanitized_drops_bad_optionals`: `version = Some("1.0 beta")` becomes `None`, and
    `tools = [("Bash",3),("../x",1)]` becomes `[("Bash",3)]`.
  - `tokens_add`: `{input:None}` + `{input:Some(5)}` gives `Some(5)`. `Some(2)` + `Some(3)` gives `Some(5)`.
- [ ] **Step 2:** Run `cargo test --test event` and expect FAIL.
- [ ] **Step 3:** Implement `src/event.rs` with the interfaces above. Write the validators by hand on bytes, with no
  regex crate.
- [ ] **Step 4:** Run `cargo test --test event` and expect PASS.
- [ ] **Step 5: Commit** `feat(event): canonical invocation model and allowlist validators`.

### Task 3: Config resolution

**Files:**
- Create: `src/config.rs`
- Test: `tests/config.rs`

**Interfaces:**
- Produces:
  ```rust
  pub enum IdentityMode { Email, Anonymous }
  pub enum Source { Env, ExeDir, UserConfig, Default }
  pub struct Config {
      pub endpoint: Option<String>, pub headers: Vec<(String, String)>,
      pub identity: IdentityMode, pub identity_salt: Option<String>,
      pub allow_override: bool, pub disabled: bool,
      pub sources: std::collections::BTreeMap<&'static str, Source>,      // keys: "endpoint","headers","identity"
  }
  pub trait Env { fn var(&self, key: &str) -> Option<String>; }
  pub struct ProcessEnv;  impl Env for ProcessEnv
  pub fn resolve(env: &dyn Env, exe_dir: &Path, home: &Path) -> Config;
  pub fn parse_headers(s: &str) -> Vec<(String, String)>;                  // "k=v,k2=v2"; '=' inside values allowed after the first; values percent-decoded (OTel format)
  pub fn file_path_user(home: &Path) -> PathBuf;                           // ~/.config/wake/config.json (respects XDG_CONFIG_HOME via env in resolve)
  #[derive(Serialize, Deserialize, Default)] pub struct FileConfig {
      pub endpoint: Option<String>, pub headers: Option<BTreeMap<String,String>>,
      pub identity: Option<String>, pub identity_salt: Option<String>, pub allow_override: Option<bool> }
  ```
- **Resolution.** Each of `endpoint`, `headers` and `identity` takes the first source that has it, in this order:
  1. env `WAKE_OTLP_ENDPOINT` / `WAKE_OTLP_HEADERS` / `WAKE_IDENTITY`, used only if `allow_override`;
  2. `exe_dir/telemetry.json`;
  3. `~/.config/wake/config.json`.

  `allow_override` and `identity_salt` come from the files only, in the same order (2 then 3).
  `allow_override` defaults to true. `disabled` is true when `WAKE_DISABLED` is `1` or `true`, regardless of
  `allow_override`. An unreadable or invalid file counts as absent. A non-`https` endpoint is accepted only when its
  host is `localhost`, `127.0.0.1` or `[::1]`; any other plain-`http` endpoint resolves to `None`.

- [ ] **Step 1: Write the failing tests**, each using a `tempfile` home and exe_dir and a `MapEnv` test helper:
  - `env_beats_files`: env endpoint, exe-dir file and user file are all set, and the result is the env value with
    source `Env`.
  - `exe_dir_beats_user_file`.
  - `allow_override_false_ignores_env`: exe-dir file has `allow_override=false`, env sets an endpoint, and the
    result is the file's endpoint.
  - `disabled_honoured_even_without_override`: `WAKE_DISABLED=1` with `allow_override=false` gives
    `disabled == true`.
  - `otel_generic_vars_ignored`: only `OTEL_EXPORTER_OTLP_ENDPOINT` is set, so `endpoint == None`.
  - `http_only_for_loopback`: `http://127.0.0.1:4318/v1/traces` is kept, and `http://example.com/x` gives `None`.
  - `parse_headers_cases`: `"Authorization=Basic%20abc=="` and `"Authorization=Basic abc=="` both give
    `[("Authorization","Basic abc==")]` (values are percent-decoded, as the OTel `k=v` format specifies; a value with
    no `%` is unchanged), and `"a=1,b=2"` gives two pairs.
  - `invalid_json_file_is_absent`: an exe-dir file containing `{` falls through to the user file.
- [ ] **Step 2:** Run `cargo test --test config` and expect FAIL.
- [ ] **Step 3:** Implement `src/config.rs`.
- [ ] **Step 4:** Run `cargo test --test config` and expect PASS.
- [ ] **Step 5: Commit** `feat(config): resolve endpoint, headers and identity from env and config files`.

### Task 4: Identity and repo

**Files:**
- Create: `src/identity.rs`, `src/repo.rs`
- Test: `tests/identity_repo.rs`

**Interfaces:**
- Consumes: `config::{Config, IdentityMode}`, `event::{UserId, is_email, is_repo}`.
- Produces:
  ```rust
  pub fn email(home: &Path, cwd: &Path) -> Option<String>;   // ~/.claude.json oauthAccount.emailAddress, else `git config user.email` run in cwd (1 s timeout); validated by is_email
  pub fn user_id(cfg: &Config, email: Option<String>) -> UserId;  // Email(e) | Hash(hex(HMAC-SHA256(salt, lowercase(e)))) | Unknown
  pub fn repo(cwd: &Path) -> Option<String>;                  // `git -C cwd remote get-url origin` (1 s timeout) → normalise
  pub fn normalise_remote(url: &str) -> Option<String>;      // pure; result validated by is_repo
  ```
- `normalise_remote` turns `git@host:group/name.git`, `ssh://git@host[:port]/g/n.git` and
  `https://[user@]host/g/n(.git)` into `host/g/n`. It strips credentials, ports and `.git`, and lowercases the host.
- `user_id` in `Anonymous` mode with no salt returns `Unknown`, never an unsalted hash.

- [ ] **Step 1: Write the failing tests:**
  - `normalise_ssh`: `git@gitlab.com:supermodularai/core/agents-wake.git` gives
    `gitlab.com/supermodularai/core/agents-wake`.
  - `normalise_https_with_creds`: `https://oauth2:tok@github.com/SupermodularAI/agents-wake` gives
    `github.com/SupermodularAI/agents-wake`. Also assert that the result does not contain `tok`.
  - `normalise_ssh_url_port`: `ssh://git@host.example:2222/a/b.git` gives `host.example/a/b`.
  - `normalise_local_path_rejected`: `/Users/x/repo.git` and `file:///x` give `None`.
  - `email_from_claude_json`: a temp home with `.claude.json` holding `{"oauthAccount":{"emailAddress":"a@b.co"}}`
    gives `Some("a@b.co")`.
  - `anonymous_hash_is_stable_and_salted`: the same email and salt give the same 64-hex string, a different salt
    gives a different one, and the result never contains `@`.
  - `anonymous_without_salt_is_unknown`.
- [ ] **Step 2:** Run `cargo test --test identity_repo` and expect FAIL.
- [ ] **Step 3:** Implement both files. Run git with `std::process::Command`, kill it after 1 s, and use stdout only.
- [ ] **Step 4:** Run `cargo test --test identity_repo` and expect PASS.
- [ ] **Step 5: Commit** `feat(identity): email or salted pseudonym, and a normalised repo name`.

### Task 5: Ids and OTLP encoding

**Files:**
- Create: `src/ids.rs`, `src/otlp.rs`
- Test: `tests/otlp.rs`

**Interfaces:**
- Consumes: `event::*`.
- Produces:
  ```rust
  pub fn trace_id(session_id: &str) -> [u8; 16];          // sha256("wake-trace\0" + session_id)[..16]
  pub fn span_id(key: &str) -> [u8; 8];                   // sha256("wake-span\0" + key)[..8]
  pub fn encode(ctx: &Context, invs: &[Invocation]) -> serde_json::Value;   // OTLP ExportTraceServiceRequest (JSON mapping)
  ```
- **Encoding (OTLP JSON mapping):**
  - Ids are lowercase hex. Times are `startTimeUnixNano` / `endTimeUnixNano` as decimal strings. `kind` is 1
    (INTERNAL).
  - Resource attributes: `service.name="wake"`, `service.version=<CARGO_PKG_VERSION>`.
  - Span name: `"wake.<kind> <name>"`.
  - Span attributes, each only when the value is present:

  | Attribute | From |
  | --- | --- |
  | `wake.harness`, `wake.harness.version` | `Context` |
  | `wake.session.id`, `session.id` | `ctx.session_id` |
  | `user.id` + `wake.user.email` / `wake.user.hash` | `ctx.user` |
  | `wake.repo` | `ctx.repo` |
  | `wake.primitive.kind`, `wake.primitive.name`, `wake.primitive.version`, `wake.plugin`, `wake.marketplace`, `wake.primitive.context_tokens` | invocation |
  | `wake.invoker`, `wake.outcome` | invocation |
  | `wake.window.duration_ms` | `(end_ns - start_ns) / 1_000_000` |
  | `wake.window.tool_calls`, `wake.window.tool_errors` | invocation |
  | `wake.window.tools` | `"Bash:6,Read:5"`: count descending, then name ascending |
  | `wake.mcp.server`, `wake.mcp.calls`, `wake.mcp.errors`, `wake.mcp.total_duration_ms`, `wake.mcp.max_duration_ms` | `mcp` |
  | `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, `gen_ai.usage.cache_read_input_tokens`, `gen_ai.usage.cache_creation_input_tokens` | model, tokens |
  | `wake.inventory` | `arrayValue` of `stringValue`s, Inventory kind only |

  - Integers use `intValue` as decimal strings. A token count is omitted when `None`, never sent as 0.

- [ ] **Step 1:** In the current Langfuse docs ("OpenTelemetry" integration):
  - Confirm that `/api/public/otel/v1/traces` accepts OTLP/HTTP **JSON** (`Content-Type: application/json`), not
    only protobuf. If it accepts protobuf only, **stop and tell the user**: the encoding choice changes, and with it
    the dependency budget.
  - Confirm the attribute names for the user, the session, token usage and the model (property mapping). If they
    differ from the table, use Langfuse's names, update the table here and in spec §5.6, and note the doc URL in the
    commit message.
- [ ] **Step 2: Write the failing tests:**
  - `ids_deterministic`: the same key gives the same id, different keys give different ids, and `trace_id` returns
    16 bytes.
  - `encode_skill_span_shape`: one skill invocation with tokens `{input:Some(48211), output:Some(6930)}`, tools
    `[("Read",5),("Bash",6)]` and model `claude-opus-5-5`. Assert:
    - the span name is `wake.skill brainstorming`;
    - `wake.window.tools == "Bash:6,Read:5"`;
    - `gen_ai.usage.input_tokens` has `intValue == "48211"`;
    - `gen_ai.usage.cache_read_input_tokens` is absent;
    - the `traceId` is 32 hex chars;
    - `parentSpanId` is absent when there is no parent.
  - `encode_email_vs_hash`: `UserId::Hash` gives `wake.user.hash` and `user.id` equal to the hash, with no
    `wake.user.email`.
  - `encode_inventory_array`: an Inventory invocation encodes `wake.inventory` as an `arrayValue` of 2 strings.
  - `encode_never_contains_forbidden_keys`: encode a fully populated invocation, serialise it to a string, and
    assert it contains none of `prompt`, `tool_input`, `command`, `/Users/`, `cwd`, `transcript`.
- [ ] **Step 3:** Run `cargo test --test otlp` and expect FAIL.
- [ ] **Step 4:** Implement `ids.rs` and `otlp.rs`.
- [ ] **Step 5:** Run `cargo test --test otlp` and expect PASS.
- [ ] **Step 6: Commit** `feat(otlp): encode invocations as OTLP JSON spans with deterministic ids`.

### Task 6: Delivery and outbox

**Files:**
- Create: `src/send.rs`
- Test: `tests/send.rs`

**Interfaces:**
- Consumes: `config::Config`.
- Produces:
  ```rust
  pub enum SendError { Network, Status(u16) }
  pub trait Transport { fn post(&self, url: &str, headers: &[(String, String)], body: &[u8]) -> Result<(), SendError>; }
  pub struct UreqTransport;            // timeout_connect 2 s, timeout 2 s total; 2xx = Ok
  pub struct Outbox { pub path: PathBuf }
  impl Outbox { pub fn len(&self) -> usize }
  pub fn deliver(t: &dyn Transport, cfg: &Config, outbox: &Outbox, payload: &serde_json::Value, now_secs: u64);
  ```
- **`deliver` rules:**
  - If `cfg.endpoint` is `None`, do nothing.
  - First retry the outbox entries oldest-first, stopping at the first failure.
  - Then send `payload`. On failure, append it.
  - Rewrite the outbox atomically (tmp file, then rename) and keep only entries newer than 7 days, capped at the
    newest 1,000.
  - A 4xx other than 408 or 429 is a permanent rejection: drop that entry, don't queue it.
  - The outbox line format is `{"t":<secs>,"body":<payload>}`.

- [ ] **Step 1: Write the failing tests**, each with a `FakeTransport` that records calls and returns scripted
  results:
  - `success_sends_once_and_outbox_stays_empty`.
  - `failure_queues_then_next_call_flushes_in_order`: the first call fails, and the second call posts the queued
    body first, then the new one.
  - `outbox_caps_at_1000_and_7_days`: pre-fill 1,005 entries, 3 of them older than 7 days, with a failing
    transport. Afterwards `len() == 1000`, the oldest are dropped and no entry is older than 7 days.
  - `permanent_4xx_dropped`: a 400 is not queued. A 429 is queued.
  - `no_endpoint_no_calls`.
  - `unreachable_endpoint_returns_quickly`: `UreqTransport` against `http://10.255.255.1:9/` (non-routable) returns
    `Err` in under 3 s. Mark the test `#[ignore]` when the `CI_NO_NET` env var is set.
- [ ] **Step 2:** Run `cargo test --test send` and expect FAIL.
- [ ] **Step 3:** Implement `src/send.rs`.
- [ ] **Step 4:** Run `cargo test --test send` and expect PASS.
- [ ] **Step 5: Commit** `feat(send): POST with timeout and a bounded outbox`.

### Task 7: Incremental transcript reader

**Files:**
- Create: `src/harness/mod.rs`, `src/harness/claude_code/transcript.rs`
- Test: `tests/transcript.rs`, `tests/fixtures/claude_code/basic.jsonl`

**Interfaces:**
- Consumes: `event::Tokens`.
- Produces:
  ```rust
  pub enum Block { ToolUse { id: String, name: String, skill: Option<String>, subagent_type: Option<String> },
                   ToolResult { tool_use_id: String, is_error: bool } }
  pub struct AgentResult { pub agent_id: String, pub agent_type: Option<String>, pub status: Option<String>,
                           pub is_async: bool,
                           pub total_duration_ms: Option<u64>, pub total_tool_use_count: Option<u64>, pub usage: Tokens }
  pub struct TaskNotification { pub tool_use_id: String, pub status: Option<String> }   // status: completed | failed | killed
  pub enum Role { User, Assistant, Other }
  pub struct Entry {
      pub role: Role, pub ts_ns: u64, pub request_id: Option<String>, pub model: Option<String>,
      pub usage: Option<Tokens>, pub version: Option<String>, pub is_sidechain: bool, pub is_meta: bool,
      pub attribution_skill: Option<String>, pub attribution_agent: Option<String>,
      pub attribution_mcp_server: Option<String>, pub attribution_plugin: Option<String>,
      pub blocks: Vec<Block>, pub command_name: Option<String>, pub interrupted: bool,
      pub agent_result: Option<AgentResult>, pub task_notification: Option<TaskNotification>,
  }
  pub fn read_from(path: &Path, offset: u64) -> std::io::Result<(Vec<Entry>, u64)>;   // returns entries and the offset just after the last complete '\n'
  ```
- **Field mapping** (spec §3):
  - `type`, `timestamp` (RFC 3339 → ns), `requestId`, `message.model`, `version`, `isSidechain`.
  - `message.usage`:
    - `input_tokens` → `input`;
    - `output_tokens` → `output`;
    - `cache_read_input_tokens` → `cache_read`;
    - `cache_creation_input_tokens` → `cache_creation`.
  - `attributionSkill`, `attributionAgent`, `attributionMcpServer`, `attributionPlugin`.
  - `message.content[]` `tool_use` blocks: `id`, `name`, `input.skill`, `input.subagent_type`.
  - `tool_result` blocks: `tool_use_id`, `is_error`.
  - `isMeta` → `is_meta`.
  - `command_name`: only on a user entry that is not `isMeta` and whose string content, or first text block, **starts**
    with `<command-name>` (after leading whitespace). It is the text up to `</command-name>`, with a leading `/`
    stripped. The literal tag appears inside skill bodies, pasted text and tool descriptions, which must not count.
  - `interrupted`: the user content contains `[Request interrupted by user`.
  - `toolUseResult`, when it has `agentId`: `agentId`, `agentType`, `status`, `isAsync` (also true when `status` is
    `async_launched`), `totalDurationMs`, `totalToolUseCount` and `usage` (same mapping as above). An async result
    has only `agentId`, `status`, `isAsync` and fields wake does not read.
  - `task_notification`: a user entry whose content starts with `<task-notification>` (Claude Code also sets
    `origin.kind == "task-notification"`). Keep only `<tool-use-id>` and `<status>`; never the summary, result or
    output-file path. The same envelope is used for background Bash tasks, so it means nothing unless its tool-use
    id matches a pending async agent (Task 10).
- **What is kept:** use serde structs with `#[serde(default)]` that name only these fields, so no other content is
  ever held. Lines that fail to parse are skipped and their bytes still counted.

- [ ] **Step 1:** Write the fixture `basic.jsonl` (about 12 lines):
  - a user prompt;
  - an assistant entry with a `Skill` tool_use (`skill: "brainstorming"`) and usage;
  - the Skill `tool_result`;
  - two assistant entries sharing one `requestId` with identical usage, both `attributionSkill: "brainstorming"`;
  - a `Bash` tool_use and its `tool_result` with `is_error: true`;
  - a user entry with `<command-name>/review</command-name>`;
  - an `isMeta` user entry, and a plain user prompt, both containing `<command-name>/fake</command-name>` mid-text;
  - a user entry with `<task-notification><tool-use-id>toolu_x</tool-use-id><status>completed</status><summary>SECRET-SUMMARY</summary></task-notification>`;
  - a user entry with `[Request interrupted by user]`.
- [ ] **Step 2: Write the failing tests:**
  - `parses_fields`: the entry count is right. The Skill block has `skill == Some("brainstorming")`, the shared
    `requestId` appears on two entries, `command_name == Some("review")` and `interrupted` is true on the last entry.
  - `command_name_only_at_start`: neither `fake` entry has a `command_name`.
  - `task_notification_parsed`: `tool_use_id == "toolu_x"`, `status == Some("completed")`, and `SECRET-SUMMARY` is not
    in `format!("{:?}", entries)`.
  - `partial_last_line_not_consumed`: append `{"type":"assistant"` with no newline. The returned offset equals the
    length before the append, and the entry count is unchanged.
  - `resume_from_offset_reads_only_new`: read to offset X, append 2 lines, and reading from X returns exactly 2
    entries.
  - `unknown_content_not_retained`: build an entry whose text content contains `SECRET-PROMPT`, and assert that
    `format!("{:?}", entries)` does not contain it.
- [ ] **Step 3:** Run `cargo test --test transcript` and expect FAIL.
- [ ] **Step 4:** Implement the reader. Seek to `offset`, use a `BufReader`, and walk line by line.
- [ ] **Step 5:** Run `cargo test --test transcript` and expect PASS.
- [ ] **Step 6: Commit** `feat(claude-code): incremental transcript reader holding allowlisted fields only`.

### Task 8: Installed primitives (lookup and inventory)

**Files:**
- Create: `src/harness/claude_code/inventory.rs`
- Modify: `src/harness/mod.rs`
- Test: `tests/inventory.rs`, `tests/fixtures/claude_code/home/...` (synthetic tree)

**Interfaces:**
- Produces (in `harness/mod.rs`):
  ```rust
  pub struct PrimitiveInfo { pub kind: Kind, pub name: String, pub version: Option<String>, pub plugin: Option<String>,
                             pub marketplace: Option<String>, pub context_tokens: Option<u64> }
  pub trait PrimitiveLookup { fn skill(&self, name: &str) -> Option<PrimitiveInfo>; fn agent(&self, name: &str) -> Option<PrimitiveInfo>; }
  ```
- Produces (in `inventory.rs`):
  ```rust
  pub struct Installed { pub items: Vec<PrimitiveInfo> }
  impl Installed { pub fn scan(home: &Path, project_dir: &Path) -> Installed; pub fn inventory_strings(&self) -> Vec<String>; }
  impl PrimitiveLookup for Installed
  ```
- **Sources** (stat and read only, with no recursion beyond what is listed):
  - `~/.claude/skills/*/SKILL.md` and `<project>/.claude/skills/*/SKILL.md`: local skills.
  - `~/.claude/agents/*.md` and `<project>/.claude/agents/*.md`: local agents.
  - `~/.claude/plugins/installed_plugins.json`. In `plugins["<plugin>@<marketplace>"][*]`, take `installPath` and
    `version`. Under `installPath`, read `skills/*/SKILL.md` (named `<plugin>:<dir>`) and `agents/*.md` (named
    `<plugin>:<stem>`), with `plugin` and `marketplace` set from the key.
  - **APM versions.** For local skills, if `<project>/apm.lock.yaml` or `~/.apm/apm.lock.yaml` lists the package that
    deployed `.claude/skills/<name>/`, set `version` from it and `plugin` to the package name. Step 1 captures the
    real format.
- `context_tokens` is the `SKILL.md` or agent file size in bytes divided by 4, rounded up.
- `inventory_strings` produces `"skill:<name>@<version or ->"` and `"agent:<name>@<version or ->"`, sorted.
- Lookup is by exact name first. Then, if the name has no `:`, it is the unique item whose name ends with
  `:<name>`.

- [ ] **Step 1:** Inspect a real `apm.lock.yaml`. Run `apm install` in a scratch dir for one `smos-*` package, at
  project scope. Write a **synthetic** fixture with the same structure.
  - If package and version can be read line by line without a YAML crate, do that.
  - Otherwise leave APM versions as `None`, add a `// TODO(plan-followup)` naming the format, and record the gap in
    the PR description. Do not add a YAML dependency without the user's approval.
- [ ] **Step 2: Write the failing tests** against the synthetic home:
  - `finds_local_and_plugin_skills`: `brainstorming` resolves via suffix to `superpowers:brainstorming` with version
    `7.0.2-22f0b463e7a5` and marketplace `anthropic-plugin-directory`.
  - `context_tokens_from_size`: a 2,840-byte `SKILL.md` gives `Some(710)`.
  - `ambiguous_suffix_returns_none`: two plugins both have `:review`, so a lookup of `review` returns `None`.
  - `inventory_strings_sorted`.
  - `apm_version_when_lockfile_present` (from Step 1's fixture), or an `#[ignore]` with a reason if Step 1 fell back.
- [ ] **Step 3:** Run `cargo test --test inventory` and expect FAIL.
- [ ] **Step 4:** Implement it.
- [ ] **Step 5:** Run `cargo test --test inventory` and expect PASS.
- [ ] **Step 6: Commit** `feat(claude-code): installed-primitive lookup and inventory`.

### Task 9: Derivation, skills

**Files:**
- Create: `src/harness/claude_code/derive.rs`, `src/state.rs` (types only, see below)
- Test: `tests/derive_skills.rs`, `tests/fixtures/claude_code/skills_*.jsonl`

**Interfaces:**
- Consumes: `transcript::Entry`, `PrimitiveLookup`, `event::*`, `ids::span_id`.
- Produces (in `state.rs`, the persistence comes in Task 11):
  ```rust
  #[derive(Serialize, Deserialize, Default, Clone)]
  pub struct OpenInvocation { pub span_key: String, pub parent_key: Option<String>, pub kind: String, pub name: String,
      pub invoker: String, pub start_ns: u64, pub last_ns: u64, pub tokens: Tokens, pub model: Option<String>,
      pub tool_calls: u64, pub tool_errors: u64, pub tools: BTreeMap<String, u64>, pub pending: Vec<String>,
      pub start_tool_use_id: Option<String>, pub outcome: Option<String>, pub from_command: bool, pub attributed: bool }
  #[derive(Serialize, Deserialize, Default, Clone)]
  pub struct PendingAgent { pub agent_id: String, pub tool_use_id: String, pub span_key: String,
      pub parent_key: Option<String>, pub name: String, pub start_ns: u64 }                    // Task 10
  #[derive(Serialize, Deserialize, Default, Clone)]
  pub struct SessionState { pub offset: u64, pub open: Vec<OpenInvocation>, pub recent_requests: Vec<String>,  // recent_requests capped at 256
      pub pending_agents: Vec<PendingAgent>,                                                    // Task 10
      pub cwd: Option<String>, pub user: Option<String>, pub repo: Option<String>,              // Task 12: cached context
      pub last_ns: u64 }                                                                        // newest entry ts seen; Task 11 GC flush
  ```
- Produces (in `derive.rs`):
  ```rust
  pub struct SubagentRef { pub agent_id: String, pub span_key: String, pub parent_key: Option<String> }  // filled in by Task 10
  pub struct Derived { pub closed: Vec<Invocation>, pub subagents: Vec<SubagentRef> }
  pub fn derive(session_id: &str, entries: &[Entry], state: &mut SessionState, lookup: &dyn PrimitiveLookup, now_ns: u64, end_of_session: bool) -> Derived;
  pub const IDLE_CLOSE_NS: u64 = 30 * 60 * 1_000_000_000;
  ```
- **Skill rules** (spec §5.5), applied entry by entry in order:
  1. **Start.**
     - A `Skill` tool_use with `skill` set starts an invocation. Its span key is the tool_use id, and its invoker is
       `model`, or `nested` when the assistant entry's `attribution_skill` names another open skill. A nested
       invocation's parent key is that outer skill's span key.
     - A user `command_name` that the lookup resolves as a skill starts an invocation with invoker `user`. Its span
       key is `"cmd:" + session_id + ":" + ts_ns` (the `session_id` argument), and it sets `from_command=true`.
     - **Merge.** If a `Skill` tool_use names a skill that has an open invocation with `from_command=true` and
       `attributed=false`, it does not start a new one. It only sets `start_tool_use_id`.
  2. **Restart.** Starting a skill closes any open invocation of the same name first.
  3. **Belonging.** An assistant entry with `attribution_skill == name` of an open skill:
     - adds its usage, only if its `request_id` is not in `recent_requests` (then push the id);
     - sets `model`, `last_ns` and `attributed=true`;
     - for each tool_use block, `tool_calls += 1`, `tools[name] += 1`, and pushes the id to `pending`.
  4. **Results.** A user `tool_result` whose id is in some open invocation's `pending` removes it from `pending`.
     `is_error` increments `tool_errors`. A `tool_result` for `start_tool_use_id` with `is_error` sets
     `outcome="error"`.
  5. **Interrupt.** `interrupted` sets `outcome="interrupted"` on every open skill with `last_ns` inside the current
     turn (any attributed entry since the last user prompt), unless it is already `error`. A turn that ends in an API
     error (`StopFailure`) is not tracked: no hook is installed for it, its slice is read at the next `Stop`, and its
     skills keep `ok` unless a tool error says otherwise. (Spec §5.5 is amended to match.)
  6. **Close.** Before processing each entry, close every open skill with `entry.ts_ns > last_ns + IDLE_CLOSE_NS`.
     At the end, with `end_of_session`, close all of them. With `now_ns`, apply the idle rule against `now_ns`.
     `state.last_ns` is set to the newest entry ts seen.
  7. **Closing builds an `Invocation`:**
     - `span_id = ids::span_id(span_key)`;
     - `end_ns = last_ns`, or `start_ns` if never attributed;
     - `outcome` defaults to `ok`;
     - metadata from `lookup.skill(name)`.
- `MCP` and `Agent` tool_uses count as tools like any other (`tools["mcp__x__y"]`, `tools["Agent"]`). Their own
  events come in Task 10.

- [ ] **Step 1:** Write the fixtures:
  - `skills_multiturn.jsonl`: one `Skill` start, attributed entries across 3 user prompts, with no other skill.
  - `skills_nested.jsonl`: skill A starts, then an A-attributed entry has a `Skill` tool_use B, then B-attributed
    entries.
  - `skills_slash.jsonl`: a `<command-name>/brainstorming` entry, attributed entries, then a `Skill` tool_use for
    `brainstorming` (the 2-in-41 case).
  - `skills_dup_request.jsonl`: two entries share a `requestId` with usage `input_tokens: 100`.
  - `skills_error_interrupt.jsonl`: a Skill tool_result with `is_error`, and a separate skill interrupted.
- [ ] **Step 2: Write the failing tests:**
  - `multiturn_is_one_invocation`: after deriving with `end_of_session=true`, there is 1 closed skill with tokens
    summed over all 3 turns.
  - `multiturn_open_across_stop`: derive turn 1 with `end_of_session=false`. 0 closed and 1 open. Derive turns 2 and
    3, then end. 1 closed.
  - `nested_parent_and_invoker`: B has `invoker=Nested` and `parent_span_id == span_id(A's key)`.
  - `slash_then_tool_use_is_one_invocation`: 1 invocation, `invoker=User`.
  - `request_id_dedup`: `tokens.input == Some(100)`, not 200.
  - `skill_load_error_outcome` and `interrupted_outcome`.
  - `idle_30min_closes`: an attributed entry, then an unrelated entry 31 minutes later, closes it with
    `end_ns = last attributed ts`.
  - `restart_same_skill_closes_previous`: 2 invocations.
- [ ] **Step 3:** Run `cargo test --test derive_skills` and expect FAIL.
- [ ] **Step 4:** Implement `derive` for skills, plus the `state.rs` types.
- [ ] **Step 5:** Run `cargo test --test derive_skills` and expect PASS.
- [ ] **Step 6: Commit** `feat(claude-code): derive skill invocations from harness attribution`.

### Task 10: Derivation, subagents, MCP and commands

**Files:**
- Modify: `src/harness/claude_code/derive.rs`
- Test: `tests/derive_others.rs`, `tests/fixtures/claude_code/others_*.jsonl`,
  `tests/fixtures/claude_code/sess-1/subagents/agent-abc.jsonl`,
  `tests/fixtures/claude_code/sess-async/subagents/agent-def.jsonl` (used by Task 12)

**Interfaces:**
- Produces:
  ```rust
  // SubagentRef is already declared in Task 9; this task populates it.
  pub fn subagent_transcript(transcript_path: &Path, session_id: &str, agent_id: &str) -> PathBuf;  // <dir of transcript>/<session_id>/subagents/agent-<agent_id>.jsonl
  ```
- **Subagents, common fields.** In every case:
  - the `Agent` tool_use is matched to its result by `tool_use_id`, never by tool name;
  - span key `"agent:" + agent_id`;
  - name = the `Agent` tool_use's `input.subagent_type`, else `agent_type` from the result, else `"unknown"`;
  - start = ts of the `Agent` tool_use;
  - parent = the open skill named by the `Agent` tool_use entry's `attribution_skill`, if there is one;
  - metadata from `lookup.agent(name)`.
- **Foreground subagent** (`agent_result` with `is_async == false`): a closed `Kind::Subagent` invocation at once.
  - end = this entry's ts;
  - `tokens = usage`, `tool_calls = total_tool_use_count`;
  - outcome `ok` if status is `completed`, else `error`;
  - pushes a `SubagentRef`.
- **Background subagent** (`is_async == true`, status `async_launched`): **no event yet, and never `error` for being
  async.** Push a `PendingAgent` to `state.pending_agents` (it survives across `Stop`s in the state file). It closes
  when either of these arrives, in this or a later slice:
  - a `task_notification` whose `tool_use_id` matches. End = that entry's ts. Outcome `ok` for `completed`,
    `interrupted` for `killed`, `error` for `failed` or anything else;
  - `end_of_session`: close with end = `state.last_ns` and outcome `interrupted`.

  On close it builds the `Kind::Subagent` invocation with tokens and tool counts left `None` (Task 12 fills them from
  the subagent's transcript), removes the `PendingAgent` and pushes a `SubagentRef`.

  For a `SubagentRef`, Task 12 derives the subagent's own transcript with a fresh `SessionState`,
  `end_of_session=true`, and its root parent key set to the subagent's span key. **Recursion stops at depth 3.** For a
  background agent, Task 12 also sums that transcript's usage (deduplicated by `requestId`) and counts its tool_uses,
  and sets them on the subagent invocation before sending.
- **MCP.** A tool_use named `mcp__<server>__<tool>` and its result are aggregated per `(server, tool, enclosing open
  skill span key, or "turn")`, with duration = result ts − tool_use ts. Aggregates are emitted at the end of every
  `derive` call, so a skill that spans several turns gets one aggregate per turn per MCP tool, each parented to the
  skill (spec §5.5 is amended to say so). They are `Kind::McpTool`, named `<server>__<tool>`:
  - `mcp` is set;
  - the parent is the enclosing skill, if there is one;
  - span key `"mcp:" + first tool_use id in the aggregate`.

  A tool_use with no result in this slice still counts as a call, with duration 0.
- **Commands.** A user `command_name` that is not a skill (the lookup returns `None`) produces a closed
  `Kind::Command`, invoker `user`, `start_ns == end_ns`, span key `"cmd:" + session_id + ":" + ts_ns`.
- **Built-in tools** never produce events of their own (§2 anti-goal).

- [ ] **Step 1:** Write the fixtures:
  - `others_subagent.jsonl`: inside skill A, an `Agent` tool_use, then a user entry with `toolUseResult`
    `{agentId:"abc", agentType:"Explore", status:"completed", totalDurationMs:5000, totalToolUseCount:7, usage:{…}}`.
  - `sess-1/subagents/agent-abc.jsonl`: contains a `Skill` start and an `mcp__notion__search` call.
  - `others_mcp.jsonl`: 3 calls to `mcp__playwright__click`, one with `is_error`, durations 100, 250 and 50 ms.
  - `others_command.jsonl`: `/clear`-style `<command-name>/compact</command-name>`.
  - `others_async_t1.jsonl` and `others_async_t2.jsonl`: turn 1 has, inside skill A, an `Agent` tool_use with
    `subagent_type: "Explore"` and `run_in_background: true`, and its `toolUseResult`
    `{agentId:"def", status:"async_launched", isAsync:true}`. Turn 2 has a `<task-notification>` for that tool-use id
    with `<status>completed</status>`, and an unrelated one for a background Bash tool-use id.
- [ ] **Step 2: Write the failing tests:**
  - `subagent_event_and_parent`: kind Subagent, name `Explore`, `tool_calls == Some(7)`, `outcome == Ok`, parent =
    A's span id, and one `SubagentRef{agent_id:"abc"}`.
  - `subagent_failed_status_is_error`: status `"failed"` gives `Outcome::Error`.
  - `async_subagent_waits_for_notification`: derive turn 1 with `end_of_session=false`. No Subagent event, one
    `PendingAgent`. Derive turn 2 with the same state. One Subagent named `Explore`, `outcome == Ok`, parent = A's span
    id, `start_ns` = turn 1's tool_use ts, one `SubagentRef{agent_id:"def"}`, and `pending_agents` empty. The Bash
    notification produces nothing.
  - `async_subagent_killed_and_session_end`: a `killed` notification gives `Interrupted`. A pending agent with no
    notification closes at `end_of_session=true` as `Interrupted`.
  - `mcp_aggregated_one_event`: 1 McpTool event with `calls == 3`, `errors == 1`, `max_duration_ms == 250`,
    `total_duration_ms == 400` and `server == "playwright"`.
  - `command_event`: 1 Command named `compact` with `invoker == User`.
  - `builtin_tools_no_events`: a slice with only Bash and Read tool_uses produces 0 events.
  - `subagent_transcript_path`: `subagent_transcript("/p/sess-1.jsonl", "sess-1", "abc")` returns
    `/p/sess-1/subagents/agent-abc.jsonl`.
- [ ] **Step 3:** Run `cargo test --test derive_others` and expect FAIL.
- [ ] **Step 4:** Implement it.
- [ ] **Step 5:** Run `cargo test --test derive_others` and expect PASS.
- [ ] **Step 6: Commit** `feat(claude-code): derive subagent, aggregated MCP and command invocations`.

### Task 11: Session state persistence, lock and GC

**Files:**
- Modify: `src/state.rs`
- Test: `tests/state.rs`

**Interfaces:**
- Consumes: the `SessionState` types from Task 9.
- Produces:
  ```rust
  pub fn state_dir(env: &dyn Env, home: &Path) -> PathBuf;               // $XDG_STATE_HOME/wake or ~/.local/state/wake
  pub struct SessionGuard { /* holds the File locked with std File::try_lock */ }
  pub fn lock_session(dir: &Path, session_id: &str) -> Option<SessionGuard>;   // None if already held (non-blocking)
  pub fn load(dir: &Path, session_id: &str) -> Option<SessionState>;
  pub fn save(dir: &Path, session_id: &str, s: &SessionState) -> std::io::Result<()>;   // atomic tmp + rename
  pub fn init_offset(source: Option<&str>, transcript_len: u64) -> u64;   // "startup" => 0, anything else or None => transcript_len
  pub fn stale_open(dir: &Path, now: SystemTime) -> Vec<(String, SessionState)>;   // sessions whose state has open invocations or pending agents and was untouched > 30 min; each one lock-able (skip if locked)
  pub fn gc(dir: &Path, now: SystemTime);                                 // delete sessions/*.json and *.lock untouched > 14 days
  ```
- `session_id` is used in file names only if it passes `is_token`. Otherwise the run does nothing.
- **Stale flush (Review Focus 8).** A session killed without `SessionEnd` leaves open invocations in its state file.
  Task 12 calls `stale_open` on every run, closes those invocations as at `end_of_session` (pending agents as
  `interrupted`), sends them with the session's cached `user` and `repo`, and saves the state with `open` and
  `pending_agents` empty. It keeps the offset, so a later `--resume` still continues and never re-sends. GC then only
  ever deletes states with nothing open.

- [ ] **Step 1: Write the failing tests:**
  - `lock_is_exclusive`: a second `lock_session` while the first guard is alive returns `None`. After the drop it
    returns `Some`.
  - `save_load_roundtrip`.
  - `init_offset_rules`: `startup` gives 0. `resume`, `fork`, `clear`, `compact` and `None` give the length.
  - `gc_removes_old_only`: set the mtime to 15 days ago (with `std::fs::File::set_modified`) and the file is
    removed. A file 1 day old is kept.
  - `stale_open_finds_idle_sessions_only`: a state with an open invocation, 31 minutes old, is returned. One 5 minutes
    old is not. One 31 minutes old with nothing open is not. One whose lock is held is not.
  - `bad_session_id_rejected`: `lock_session(dir, "../x")` returns `None` and creates no file.
- [ ] **Step 2:** Run `cargo test --test state` and expect FAIL.
- [ ] **Step 3:** Implement it.
- [ ] **Step 4:** Run `cargo test --test state` and expect PASS.
- [ ] **Step 5: Commit** `feat(state): per-session state with a non-blocking lock and 14-day GC`.

### Task 12: Hook orchestration (`wake emit`) end to end

**Files:**
- Modify: `src/harness/claude_code/mod.rs`, `src/main.rs`
- Test: `tests/emit_e2e.rs`, `tests/perf.rs`

**Interfaces:**
- Consumes: everything above.
- Produces:
  ```rust
  pub struct Deps<'a> { pub env: &'a dyn Env, pub home: PathBuf, pub exe_dir: PathBuf, pub transport: &'a dyn Transport, pub now: SystemTime }
  pub fn run_hook(hook: &str, stdin: &[u8], deps: &Deps) ;       // never panics
  ```
- **Flow:**
  1. Resolve the config. Return if `disabled` or there is no endpoint.
  2. Parse the payload: `session_id`, `transcript_path`, `cwd`, `source`. Return if anything required is missing or
     `session_id` fails `is_token`.
  3. Flush `stale_open` sessions (Task 11) for **other** session ids, then `gc` the state dir. `lock_session`,
     returning if it is held.
  4. Per hook:
     - **`session-start`:** build an Inventory invocation from `Installed::scan(home, cwd)`, with span key
       `"inv:" + session_id + ":" + now`. Create the state, using `init_offset(source, len)`, if it is missing or
       `source == "fork"`.
     - **`stop`:** load the state, or init it with `init_offset(None, len)` if missing, which means skip. Run
       `read_from(offset)`, then `derive(session_id, …, end_of_session=false)`. Recurse into `SubagentRef`s, up to
       depth 3, at `<transcript dir>/<session_id>/subagents/agent-<id>.jsonl` (layout confirmed on real sessions).
       A background agent's tokens and tool count come from that recursion (Task 10).
     - **`session-end`:** the same as `stop` with `end_of_session=true`. Keep the state file.
  5. Build the `Context`:
     - harness `"claude-code"`;
     - `harness_version` from the last entry's `version`;
     - user and repo from the state's cache. Only when the cache is empty, or `cwd` changed, run
       `identity::user_id` and `repo::repo(cwd)` and store the results (this keeps `git` subprocesses off most `Stop`
       runs, and out of the perf budget).

     Sanitise everything, `encode`, then `deliver` if there are any invocations. Save the state, with the new offset,
     the open invocations and the pending agents.
- `main.rs` reads stdin, constructs `Deps` with `ProcessEnv`, `UreqTransport` and the `current_exe` dir, and calls
  `run_hook` inside `std::panic::catch_unwind`. It always exits 0.

- [ ] **Step 1: Write the failing tests**, each with a temp home, a copied fixture transcript, an exe-dir
  `telemetry.json` pointing at `http://127.0.0.1:1/` and a `FakeTransport`:
  - `stop_then_end_sends_expected_spans`: copy `skills_multiturn.jsonl` and run `session-start(source=startup)` →
    `stop` → `session-end`. Across all bodies, there is exactly 1 `wake.skill` span and 1 `wake.inventory` span. The
    skill span has `wake.harness == "claude-code"`.
  - `second_stop_sends_nothing_new`: `stop` twice with no file change. The second call posts 0 bodies.
  - `missing_state_skips_history`: no `session-start`, and `stop` on a fixture with a complete closed skill. 0 skill
    spans are posted, and the state is saved with offset = file length.
  - `resume_does_not_resend`: `session-start(source=resume)` on an already processed file, then `stop`. 0 spans.
  - `wake_disabled_posts_nothing`: `WAKE_DISABLED=1` gives 0 calls and creates no state files.
  - `concurrent_stop_one_wins`: hold the session lock in the test, run `stop`, and it posts 0 bodies and changes
    nothing.
  - `subagent_recursion`: with the `sess-1` fixtures, the posted spans include the Subagent and, parented under it,
    the subagent's skill and the MCP aggregate.
  - `async_subagent_across_stops`: turn 1 (`others_async_t1.jsonl`) then `stop` posts no Subagent span. Append turn
    2, `stop` again: one Subagent span with tokens summed from its subagent transcript fixture.
  - `stale_session_flushed`: a state file for another session with one open skill, mtime 31 minutes ago. A `stop` on
    the current session posts that skill's span with the cached user, and the other state's `open` is empty after.
  - `identity_cached`: two `stop`s on the same session run `git` at most once (count via a fake `git` on `PATH`
    that appends to a file).
  - `cli_emit_silent_exit_zero` (spawns the real binary with `HOME` set to the temp home): stdout and stderr are
    empty and the exit code is 0.
- [ ] **Step 2:** In `tests/perf.rs`, `#[ignore]`-gated and run in CI with `--ignored`:
  - generate a 20 MB synthetic transcript (about 40k entries, 200 skill invocations);
  - spawn the release binary as `wake emit stop`, with a state file whose offset is just before the last turn (a
    one-turn slice of about 1 MB). Do this once more with offset 0 to bound the worst case;
  - read the child's CPU time and max RSS with `libc::getrusage(RUSAGE_CHILDREN)` (`libc` as a dev-dependency). Assert
    the one-turn run is under 50 ms CPU and under 10 MB RSS, and the offset-0 run is under 500 ms.

  Run it with `cargo test --release --test perf -- --ignored`.
- [ ] **Step 3:** Run `cargo test --test emit_e2e` and expect FAIL.
- [ ] **Step 4:** Implement `run_hook` and wire `main`.
- [ ] **Step 5:** Run `cargo test` and `cargo test --release --test perf -- --ignored`, and expect PASS for both.
- [ ] **Step 6: Commit** `feat(claude-code): wire hooks to derive, encode and deliver`.

### Task 13: `wake check`

**Files:**
- Create: `src/check.rs`
- Modify: `src/main.rs`
- Test: `tests/check.rs`

**Interfaces:**
- Produces: `pub fn run(deps: &Deps, out: &mut dyn Write) -> i32` (exit code).
- **Output lines**, in order:
  - `endpoint: <scheme>://<host>[:port] (from <source>)`, with no path, no query and never any header values;
  - `headers: <n> set (from <source>)`;
  - `identity: email|anonymous (from <source>)`, then the resolved user printed as `email found` / `hash` /
    `unknown`;
  - `disabled: yes|no`;
  - `test event: sent|failed (<reason>)`.

  The test event is one span `wake.check`, `kind=command`, `name=check`. The exit code is 0 if it was sent, 1
  otherwise. If disabled, it says so and exits 0 without sending.

- [ ] **Step 1: Write the failing tests:**
  - `check_never_prints_secrets`: with headers `Authorization=Basic c2VjcmV0`, the output does not contain
    `c2VjcmV0`, `Basic` or the endpoint path.
  - `check_reports_sources`: an env endpoint gives `(from env)`.
  - `check_send_result`: a fake transport returning `Ok` gives `sent` and 0. `Err` gives `failed` and 1.
- [ ] **Step 2:** Run `cargo test --test check` and expect FAIL.
- [ ] **Step 3:** Implement and wire it.
- [ ] **Step 4:** Run `cargo test --test check` and expect PASS.
- [ ] **Step 5: Commit** `feat(check): print resolved config and send a test event`.

### Task 14: `wake install` / `wake uninstall` (open-source path)

**Files:**
- Create: `src/install.rs`
- Modify: `src/main.rs`
- Test: `tests/install.rs`

**Interfaces:**
- Produces:
  ```rust
  pub struct InstallArgs { pub endpoint: Option<String>, pub headers: Vec<(String,String)>, pub identity: Option<String>, pub harness: String }
  pub fn install(home: &Path, exe: &Path, args: &InstallArgs, prompt: &mut dyn Prompter) -> Result<String, InstallError>;  // returns a human summary
  pub fn uninstall(home: &Path, exe: &Path, purge: bool) -> Result<String, InstallError>;
  pub trait Prompter { fn ask(&mut self, q: &str, secret: bool) -> Option<String>; }   // terminal impl uses rpassword-free raw read: echo off via `stty -echo` for secret
  pub enum InstallError { SettingsUnreadable, SettingsNotObject, SettingsIsSymlink, UnsupportedHarness, Io(std::io::Error) }
  ```
- **Hook commands** are exactly `"<abs exe path>" emit session-start`, `… emit stop` and `… emit session-end`. Each
  group is `{"hooks":[{"type":"command","async":true,"command":…}]}`, appended under `hooks.SessionStart`,
  `hooks.Stop` and `hooks.SessionEnd`.
- **Ownership:** a group is wake's when it has exactly one hook whose `command` equals one of those strings. With any
  older exe path, the match is `ends_with(" emit <event>")` and the program path's file name is `wake` (strip
  surrounding quotes first). **Never** a command whose program path contains `/.claude/hooks/` or `${PLUGIN_ROOT}`:
  those are APM-installed, and APM owns them (Review Focus 9).
- **`install`:**
  - refuses non-`claude-code` harnesses;
  - refuses with no change for a symlinked, unparsable or non-object `settings.json`. A missing file is created as
    `{}`;
  - writes `config.json` (0600, directories 0700) from the args or prompts, and keeps existing keys that were not
    given;
  - removes any existing wake groups, then appends the new ones;
  - writes `settings.json` atomically, preserving key order (`serde_json` `preserve_order` feature), 2-space
    indentation, and whether the file ended with a trailing newline.
- **`uninstall`:** removes only wake groups. It drops event keys and the `hooks` key if they become empty and
  weren't empty before. With `purge`, it also removes `~/.config/wake` and the state dir.

- [ ] **Step 1: Write the failing tests:**
  - `install_into_empty`: with no `settings.json`, 3 groups exist afterwards, each with `async == true` and an
    absolute command path.
  - `install_preserves_others_and_order`: a settings file with other hooks, `env` and `permissions`. All of it is
    unchanged in order and content, and only 3 groups are added.
  - `install_is_idempotent`: running it twice still gives 3 groups.
  - `install_replaces_old_exe_path`: a prior group with `/old/wake emit stop` is replaced.
  - `refuses_comments_and_symlink`: settings with `// x` give `SettingsUnreadable`, and a symlinked settings file
    gives `SettingsIsSymlink`. The file bytes are unchanged in both cases.
  - `config_written_0600_without_echoing_secret`: the summary string does not contain the secret header value.
  - `uninstall_restores_bytes`: install then uninstall gives bytes identical to the original, for two fixtures
    formatted the way Claude Code writes the file (`JSON.stringify(obj, null, 2)`, non-ASCII left unescaped): one
    with a trailing newline and one without. This mirrors the spike check.
  - `uninstall_keeps_user_edited_group`: a wake group where the user added a second hook is kept.
  - `apm_hooks_untouched`: a settings file with `"$HOME/.claude/hooks/smos-telemetry/bin/wake" emit stop`. Install
    then uninstall leaves that group in place, and install still adds its own 3 groups.
- [ ] **Step 2:** Run `cargo test --test install` and expect FAIL.
- [ ] **Step 3:** Implement it. Add `serde_json = { version = "1", features = ["preserve_order"] }`. That is a
  feature, not a new crate.
- [ ] **Step 4:** Run `cargo test --test install` and expect PASS.
- [ ] **Step 5: Commit** `feat(install): add and remove Claude Code hooks and the user config`.

### Task 15: Release, install script, package template and docs

**Files:**
- Create: `.github/workflows/release.yml`, `install.sh`, `package/apm.yml`, `package/hooks/hooks.json`,
  `package/bin/wake` (the launcher), `package/bin/telemetry.example.json`, `package/README.md`,
  `docs/langfuse-setup.md`
- Modify: `README.md`, `CONTRIBUTING.md`, `CHANGELOG.md`
- Test: `tests/launcher.sh`, run from CI

**Interfaces:**
- **Release:**
  - triggered by a `v*` tag;
  - builds with `cargo-zigbuild` for `aarch64-apple-darwin`, `x86_64-apple-darwin`, `aarch64-unknown-linux-musl`
    and `x86_64-unknown-linux-musl`;
  - asset names `wake-darwin-arm64`, `wake-darwin-x64`, `wake-linux-arm64`, `wake-linux-x64`, plus
    `checksums.txt` (SHA-256);
  - fails if any binary is over 2 MB.
- **`install.sh`:** POSIX sh. Detects the platform, downloads the latest release asset and `checksums.txt`, verifies
  them, installs to `${WAKE_INSTALL_DIR:-$HOME/.local/bin}/wake` with mode 0755, prints a PATH hint and
  `Next: wake install`. It never uses sudo. On a checksum mismatch it installs nothing and exits 1.
- **`package/`:** the APM template, matching spec §5.2 and the spike's findings exactly.
  - Hooks use `"${PLUGIN_ROOT}/bin/wake" emit <event>` with `"async": true`.
  - `telemetry.example.json` lives in `bin/`.
  - The launcher is the spike's, with an unknown platform doing `cat >/dev/null; exit 0`.
- **`docs/langfuse-setup.md`:**
  - create a dedicated project;
  - make the keys and the Basic header (`base64(pk:sk)`);
  - the endpoint `https://<host>/api/public/otel/v1/traces`;
  - `wake install` or the APM `telemetry.json`;
  - `wake check`;
  - the three conditions from spec §5.8.
- **`README.md`:** rewritten for v2.
  - what it sends and what it never sends (the spec §5.6 table);
  - install on both paths;
  - the `WAKE_*` variables;
  - `WAKE_DISABLED` at three levels;
  - uninstall.

- [ ] **Step 1: Write the failing test** `tests/launcher.sh`:
  - with a fake `uname` on PATH returning `Plan9 x`, `package/bin/wake emit stop < /dev/null` exits 0 with no
    output;
  - with `Darwin arm64` and a stub `wake-darwin-arm64` that echoes `ok`, the output is `ok`.
- [ ] **Step 2:** Run `sh tests/launcher.sh` and expect FAIL (no launcher yet).
- [ ] **Step 3:** Write the files above. Add `sh tests/launcher.sh` and `shellcheck install.sh package/bin/wake` to
  `ci.yml`.
- [ ] **Step 4:** Run `sh tests/launcher.sh && shellcheck install.sh package/bin/wake`, and expect PASS.
- [ ] **Step 5:** Run `act` or `workflow_dispatch` in dry-run mode for `release.yml`, if available. Otherwise review
  it against `cargo zigbuild --release --target aarch64-unknown-linux-musl` run locally. Expected: the binary is
  produced and is under 2 MB.
- [ ] **Step 6: Commit** `feat(release): GitHub Releases, install.sh, APM package template and v2 docs`.

### Task 16: Real-session verification

**Files:** none new. This task verifies and reports.

- [ ] **Step 1:** `cargo build --release`. Back up `~/.claude/settings.json` to the scratchpad.
- [ ] **Step 2:** Start a local OTLP receiver on `127.0.0.1:4318` that logs bodies (the spike's `recv.py`). Run
  `target/release/wake install --endpoint http://127.0.0.1:4318/v1/traces`.
- [ ] **Step 3:** In a scratch directory, run `claude -p` with a prompt that invokes a skill, e.g. `"/<an installed
  skill> …"`, then one natural-language prompt that makes the model call a skill and one MCP tool. Expected in the
  receiver:
  - `wake.inventory` at start;
  - `wake.skill <name>` with `wake.invoker`, tokens and `wake.window.tools`;
  - a `wake.mcp_tool` aggregate;
  - **no** string matching the prompt text, `/Users/` or the scratch path.

  Grep the bodies to prove the last point.
- [ ] **Step 4:** Point it at a test Langfuse project with `wake install --endpoint https://<langfuse>/api/public/otel/v1/traces --header "Authorization=Basic …"`.
  Repeat one session, and confirm in the Langfuse UI that the trace appears, with user and session populated and
  cost computed from the tokens. Record any attribute that Langfuse did not map, and fix it in `otlp.rs` with a test.
  Then confirm the duplicate behaviour spec §5.8 relies on: POST one captured body twice (the second time with one
  attribute value changed), and check that Langfuse shows **one** observation for that span id, carrying the second
  value. If it shows two, report it: spec §5.8's duplicate handling then needs a redesign.
- [ ] **Step 4b:** In a session, launch a background subagent (`run_in_background`) and let it finish in a later
  turn. Confirm one `wake.subagent` span with `wake.outcome == "ok"` and tokens, sent after the notification turn.
- [ ] **Step 5:** Run `wake uninstall --purge`, and check that `~/.claude/settings.json` is byte-identical to the
  backup.
- [ ] **Step 6:** Report the results to the user, including Langfuse screenshots or notes. Do not tag a release:
  the user decides when to push `v2.0.0`.
