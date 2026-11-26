# Wake v2: design

Date: 2026-11-26. Status: draft for approval. Author: José Morais, with Claude.

## 1. Intent

Wake v2 is an **open-source** background collector for skill telemetry across a team. Anyone can install it on
their machine, point it at their own OTLP endpoint, and get metrics. From then on it sends one event per primitive
invocation (skill, subagent, MCP tool or slash command) over OTLP.

**Open source first, APM as one distribution channel (decided).** The project knows nothing about APM. It ships
binaries on GitHub Releases, an `install.sh`, and `wake install` / `wake uninstall`, which set up the harness hooks
and the config. Supermodular, and each client, distribute a release internally through their own private APM
package with their organisation's config baked in. There, APM installs the hooks and nobody runs `wake install`.
The binary is the same on both paths.

**Supermodular deployment model (decided):** one copy of the package per organisation, each configured for that
organisation's own Langfuse. Supermodular's marketplace sends only Supermodular's telemetry to Supermodular's Langfuse. A client
gets its own copy in its own marketplace, pointing at its own Langfuse. Supermodular never collects client metrics.
Langfuse is the only backend for now.

**Who it is for:** the people who own skills and the platform team. They need to know whether a skill is worth what
it costs. That means who uses it, how often, how much it costs per run, how often it fails, and whether a new version
is better than the last one. The metric set follows Uber's skill telemetry post ("How Uber Evals 1 Million Agent
Skill Invocations a Month", Nov 2026): usage, reach, retention, co-use, cost, context, latency, errors, tools,
versions, agent and installs.

**Who it is not for:** the developer whose machine it runs on. The developer gets no local view and no commands to
run. To them the tool should be close to invisible.

**Why a rewrite:** v1 (Go, about 22k production lines, 47% of them comments) was built for a different job: a local,
per-developer view with per-project consent. Several decisions made it heavy:

- It installs only `SessionStart`/`SessionEnd` hooks, and each one rescans every transcript in full. There is no
  incremental cursor.
- Events go to an append-only NDJSON spool. Duplicates are checked by re-reading the spool, and reports aggregate it
  in memory. Every schema change (9 so far) means discarding the spool and rebuilding from transcripts. History the
  harness has already pruned cannot be rebuilt.
- About 4.6k lines handle consent, the HMAC repository identity and the global boundary.
- It ships its own self-update, uninstall, dashboard, doctor and remote-delivery CLI.

v2 drops all of that. The only v1 idea it keeps is that every event is built from a closed allowlist of fields.

## 2. Goals, non-goals, anti-goals

**Goals**
- Produce all 12 metrics above, across a team, in Langfuse.
- Use one canonical event for every harness, with `harness` as an attribute. Claude Code comes first. Pi and Codex
  follow if their extension points make it simple.
- Install in one step on either path: `install.sh` + `wake install` for anyone, or the organisation's APM package with
  no per-developer setup. Telemetry starts as soon as either is done.
- Send developer identity as the real email by default, with an org-level flag to pseudonymise it.

**Non-goals**
- Any local view: no report, dashboard or doctor.
- Any developer-facing CLI beyond setup. `install` and `uninstall` exist, and nothing like v1's report, serve, doctor,
  update or remote. Updates come from re-running `install.sh` or from the APM package.
- Importing history, or rescanning past sessions.
- Per-project consent, collection boundaries, worktree identity.
- Evals, ablation and agent-filed papercuts. These are separate systems that may consume this data later.
- Computing cost in USD. The binary sends tokens and model, and the backend prices them.
- Windows.
- Integrating Claude Code's native OTel, which needs MDM. That is the future "option C" in §4.
- Grafana, an OTel Collector, and multi-tenant delivery in v2.0. One organisation sends to one Langfuse. The
  Grafana/Prometheus deployment is written up when the first client that needs it arrives (§5.10).

**Portability rule (hard constraint):** the binary must not depend on anything only Langfuse does. It speaks generic
OTLP/HTTP JSON with OpenTelemetry semantic conventions (`gen_ai.*`, `user.id`, `session.id`) plus `wake.*`
attributes. Pricing and duplicate handling belong to the backend. Any organisation can point its copy of the
package at its own OTLP endpoint (Langfuse, an OTel Collector, Grafana Alloy) without a code change.

**Anti-goals (a failure even if it works)**
- The developer notices it: added latency, prompts, or output in the session.
- Any content leaves the machine: prompts, tool inputs or outputs, Bash commands, file paths, local repo paths.
- One network event per built-in tool call. The volume would be noise, and it is not the unit owners care about.
- Growing back into v1: local stores, rebuild logic, consent machinery.

## 3. Constraints and verified facts

These were verified on 2026-11-26 against the Claude Code docs and real local transcripts (178 files).

- **Hooks** (code.claude.com/docs/en/hooks): plugins ship `hooks/hooks.json`, which is merged with user hooks.
  `${CLAUDE_PLUGIN_ROOT}` resolves in a hook `command`, and `CLAUDE_PLUGIN_DATA` is exported to the hook process.
  `"async": true` runs the hook in the background, so it never blocks the session.
- **Common hook input:** `session_id`, `prompt_id`, `transcript_path`, `cwd`, `hook_event_name`. Hooks running
  inside a subagent also get `agent_id` and `agent_type`.
- **Stop:** fires at the end of each assistant turn.
- **SubagentStop:** carries `agent_id`, `agent_type` and `agent_transcript_path`.
- **SessionStart:** carries `source` (startup | resume | clear | compact | fork).
- **SessionEnd:** carries `reason`. Its synchronous budget is 1.5 s, which an async hook avoids.
- **Transcript shape** (observed in real files):
  - Assistant entries hold `message.content[]` blocks and `message.usage`. The usage fields are `input_tokens`,
    `output_tokens`, `cache_read_input_tokens`, `cache_creation_input_tokens` and `output_tokens_details`. Each entry
    also has `requestId`, `timestamp`, `sessionId`, `isSidechain` and `version`.
  - Skill tool: a `tool_use` block with `name: "Skill"` and `input.skill`.
  - Subagent tool: `name: "Agent"` with `input.subagent_type`.
  - MCP tools: named `mcp__<server>__<tool>`.
  - Tool results: `tool_result` blocks with `tool_use_id` and `is_error`.
  - Slash commands: user entries containing `<command-name>`, `<command-message>` and `<command-args>` tags.
  - **Usage is repeated** across entries that share a `requestId`. Tokens must be deduplicated by `requestId`, or
    they are double-counted.
  - **The harness attributes every request itself.** Each assistant entry that carries `usage` also carries
    `attributionSkill`, `attributionAgent`, `attributionMcpServer`, `attributionMcpTool` and `attributionPlugin`
    (strings; 1,463 entries with `attributionSkill` observed). Wake uses these attributes and does not infer skill
    windows of its own.
  - **Slash-invoked skills rarely produce a `Skill` tool_use.** Only 2 of the 41 observed did. Their content is
    injected as user entries (`isMeta`). An invocation therefore starts at either a `Skill` tool_use or a
    `<command-name>` entry.
  - **The `Agent` tool result** (`toolUseResult` on the user entry) of a foreground subagent carries `agentId`,
    `agentType`, `status`, `usage`, `totalTokens`, `totalToolUseCount`, `toolStats` and `totalDurationMs`. The
    subagent's own transcript is at `<session dir>/subagents/agent-<agentId>.jsonl`.
  - **Background subagents are the common case.** In a local sample, 45 of 55 `Agent` results had
    `status: "async_launched"` (`run_in_background`). That result carries only `agentId`, `isAsync`, `status` and
    fields wake does not read: no usage, no type, no duration. The subagent is still running, and its transcript is
    still being written, when the parent turn's `Stop` fires. Its end arrives later as a user entry whose content
    starts with `<task-notification>` (`origin.kind == "task-notification"`), carrying `<tool-use-id>` (the `Agent`
    tool_use) and `<status>` (`completed`, `failed` or `killed`). Background Bash tasks use the same envelope.
  - **`<command-name>` also appears inside other text** (skill bodies, pasted text, tool descriptions). Only a
    non-`isMeta` user entry whose content starts with the tag is a slash command.
- **Identity:** `~/.claude.json` has `oauthAccount.emailAddress` for subscription logins.
- **Distribution:** the Supermodular marketplace is an APM catalog
  (`gitlab.com/supermodularai/core/transformation-stack/ai-primitives/marketplace`). Packages are installed from git
  tags with `apm install <pkg>@smos --target claude`. Binaries in the package therefore stay in git history
  permanently.
- **Binary size,** measured for a minimal prototype that parses JSON and POSTs over HTTPS:

  | Build | darwin-arm64 |
  | --- | --- |
  | Rust (ureq + rustls, opt-level z, LTO, strip) | 1.25 MB |
  | Go 1.25 (`-s -w`, static) | 5.6 MB |

**APM distribution spike (2026-11-26, APM 0.36.0, Claude Code headless sessions).** A throwaway package (a Rust
mock binary, 4 platform slots, a sh launcher, async hooks) was installed with `apm install -g --target claude`, and
real sessions were run against a local OTLP receiver. Results:

- **APM installs hooks, not a plugin.**
  - It merges the hooks into `~/.claude/settings.json`. Each group gets `"matcher": "*"`, and `"async": true` is
    kept.
  - It rewrites `${PLUGIN_ROOT}` to `$HOME/.claude/hooks/<package>/`. Packages must use `${PLUGIN_ROOT}`, as
    `smos-infra` does, not `${CLAUDE_PLUGIN_ROOT}`.
- **Only the directory a hook command points at is copied.** `bin/` arrived with every file and the exec bits
  intact. A `telemetry.json` at the package root was **not** copied, and was found only under `~/.apm/apm_modules/`.
  **`telemetry.json` must live in `bin/`, next to the binary.**
- **No `CLAUDE_PLUGIN_ROOT` or `CLAUDE_PLUGIN_DATA`** reaches the hook. Only `CLAUDE_PROJECT_DIR` does. The data dir
  is therefore wake's own: `~/.local/state/wake`, or `$XDG_STATE_HOME/wake`.
- **All three hooks fired** (`SessionStart`, `Stop`, `SessionEnd`), ran the binary, and their spans reached the
  receiver.
  - `Stop` payload keys: `session_id`, `prompt_id`, `transcript_path`, `cwd`, `effort`, `permission_mode`,
    `last_assistant_message`, `stop_hook_active`, `background_tasks`, `session_crons`.
- **Update** (reinstall at a new tag) replaced the binaries and `telemetry.json`. The hooks were "adopted", not
  duplicated, so there were still 3 entries.
- **`WAKE_DISABLED=1`** stopped all sending, both as a shell export and from a repo's `.claude/settings.local.json`
  `env`.
- **Uninstall** (`apm uninstall -g`) removed the hook entries and `~/.claude/hooks/<package>/`, and left
  `~/.claude/settings.json` byte-identical to its pre-install backup.
- **Size:** 4 × 1.27 MB binaries installed without complaint (4.8 MB on disk).
- **opencode:** APM reports user-scope hooks as *not supported* for opencode. A later opencode adapter needs another
  path, such as an opencode plugin.

**Still unverified:**
- The `<package>` directory name when installing from a git remote through the marketplace. The spike installed from
  a local path, so the directory was named after it (`pkg`).
- The extension points in Pi and Codex.

## 4. Approaches considered

| | Approach | Why not or why |
| --- | --- | --- |
| A | Only configuration: each harness's native OTel goes to a central collector, which normalises it | Needs MDM: a plugin cannot set env vars, and repo settings cannot enable telemetry. Content leaves the machine before it is scrubbed, and custom skill names need `OTEL_LOG_TOOL_DETAILS=1`, which also exports Bash commands and paths. Normalising several harnesses with collector rules is brittle. |
| **B** | **A small binary called by each harness's hooks emits canonical events (chosen)** | It installs from the marketplace, enforces the allowlist on the machine, and gives every harness the same event. The cost is maintaining one thin adapter per harness. |
| C | B, plus Claude Code native OTel for cost where MDM exists, joined in the backend on `session.id` | More precise Claude Code cost, but two pipelines and an MDM dependency. It can be added later on top of B without changing B. |

**Language: Rust (chosen) over Go.** No language with a runtime qualifies, because the tool must work on any dev
machine without asking for Node or Python. Between Go and Rust, binary size decides it. The package lives in git
history and carries 4 binaries per release: about 5 MB with Rust against about 23 MB with Go. Hooks are async, so
startup time does not matter. The accepted cost is harder cross-compilation (solved once in CI) and less team
familiarity with Rust.

## 5. Design

### 5.1 Shape

```
Claude Code ──hooks (async)──▶ wake emit <event> ──OTLP/HTTP JSON──▶ the org's Langfuse
 (Pi, Codex later)               reads stdin + transcript slice      (/api/public/otel/v1/traces)
```

### 5.2 The package

**Two repositories (decided).** The open-source project and Supermodular's distribution of it are kept apart.
`telemetry.json` carries Langfuse keys, so it cannot live in the public source repo.

1. **Open-source project:** `github.com/SupermodularAI/agents-wake`, public, MIT. v2 replaces v1 in this repo. It
   holds:
   - the Rust crate, tests and docs;
   - `install.sh`;
   - an APM package template under `package/`, with `telemetry.example.json` only, for any organisation that wants to
     distribute through APM;
   - the release workflow, which cross-compiles the 4 binaries with `cargo-zigbuild` and publishes them, with
     checksums, as a **GitHub Release**.

   `install.sh` downloads the right binary from the latest release, verifies its SHA-256, and puts it in
   `~/.local/bin`, as v1's installer did. The project's job ends at the release. It knows nothing about any
   organisation's distribution.
2. **Supermodular's distribution:** `gitlab.com/supermodularai/core/transformation-stack/ai-primitives/smos-telemetry`,
   private. It is what Supermodular's marketplace `apm.yml` lists. Its own CI takes a pinned agents-wake release,
   verifies the checksums, and commits the binaries next to Supermodular's `telemetry.json`. A client does the same in
   its own private repo. It holds no code, only what gets installed:

```
apm.yml
.claude-plugin/plugin.json
hooks/hooks.json
bin/wake                      # POSIX sh launcher: picks the binary by uname -s / uname -m
bin/wake-darwin-arm64
bin/wake-darwin-x64
bin/wake-linux-arm64
bin/wake-linux-x64
bin/telemetry.json            # org config, next to the binary: APM only copies the hook's directory (§3 spike)
```

`hooks.json` references the launcher as `"${PLUGIN_ROOT}/bin/wake" emit <event>`. The binary finds
`telemetry.json` in its own directory.

**Developer configuration on the APM path is environment variables only.** That means `WAKE_OTLP_ENDPOINT`,
`WAKE_OTLP_HEADERS`, `WAKE_IDENTITY` and `WAKE_DISABLED`, set in the shell or in the `env` block of
`~/.claude/settings.json`. Editing the installed `telemetry.json` is unsupported, because the next APM update
silently overwrites it. On the open-source path, `wake install` writes `~/.config/wake/config.json`, and re-running
it changes the config.

A client gets its own private package repo with the same layout and its own `telemetry.json`.

Source crate layout (indicative):

```
src/main.rs  config.rs  identity.rs  repo.rs  event.rs  otlp.rs  send.rs  state.rs
src/harness/mod.rs                      # the contract each harness implements
src/harness/claude_code/{mod,transcript,derive,inventory}.rs
tests/{golden,allowlist,perf}.rs  tests/fixtures/claude_code/*.jsonl
```

Expected size: 1.5–2.5k production lines, against 22k in v1.

**Installation (decided).**

- **Open-source path:** `curl -fsSL …/install.sh | sh`, then `wake install`. Removal is `wake uninstall`.
- **APM path:** always user-level, `apm install smos-telemetry@smos --target claude -g`. A project-scope install (no
  `-g`) is wrong for this tool, for two reasons. It would collect only in that repo. And it would write the hooks
  into that repo's config, so they would run for everyone who clones it, including people who never chose to
  install. Removal is `apm uninstall -g smos-telemetry`.

**Turning it off (decided):** `WAKE_DISABLED=1` makes `wake emit` exit immediately and do nothing. Claude Code passes
`env` from settings to hooks, so the switch works at three levels:
- a shell export, to switch everything off temporarily;
- `~/.claude/settings.json`, to switch off permanently without uninstalling;
- a repo's `.claude/settings.local.json`, to switch off for that repo only, for example client work.

`WAKE_DISABLED` is honoured even when `allow_override=false`. It never sends a "disabled" signal.

**Transparency:** the event carries real email, so the organisation must tell its developers that the telemetry
exists, what it sends and how to switch it off. That is an internal announcement, not an in-tool prompt, so the tool
stays invisible.

### 5.3 The binary

The binary has four subcommands:

- `wake emit <hook-event>` reads the hook payload from stdin. It derives events, sends them and exits 0, even on
  errors. It never writes to stdout or stderr in a way the harness would show the user.
- `wake check` is for support. It prints the resolved config (endpoint host, where each value came from, identity
  mode), sends one test event, and reports success or failure.
- `wake install [--harness claude-code] [--endpoint URL] [--header K=V]... [--identity email|anonymous]` is for the
  open-source path.
  - **Config:** it writes `~/.config/wake/config.json` (mode 0600, because it holds credentials) from the flags. At a
    terminal it prompts for missing values. The secret is never echoed.
  - **Hooks:** it merges the three hook groups into `~/.claude/settings.json`, pointing at the installed binary's
    absolute path, with `"async": true`. Every other hook and setting is left untouched.
  - **Ownership:** wake recognises its own groups by an exact command match: the binary path followed by
    `emit <event>`.
  - **Safety:** it is idempotent, so re-running updates rather than duplicates. It refuses, and writes nothing, if
    `settings.json` cannot be parsed.
  - **Later harnesses:** `--harness` is how Pi and Codex get added. Each writes its own harness's config.
- `wake uninstall [--harness …]` removes exactly the groups wake owns and leaves `settings.json` otherwise
  byte-identical. With `--purge` it also deletes `~/.config/wake` and `~/.local/state/wake`. It does not delete the
  binary, because `install.sh` put it there and removing it is one `rm`.

`install` and `uninstall` are only for the open-source path. Under APM, the package's `hooks.json` and APM's own
install and uninstall do this job, and the spike verified that (§3).

**Config resolution (both paths).** Each key is resolved separately, highest priority first:

1. `WAKE_*` environment variables (subject to `allow_override`);
2. `telemetry.json` next to the binary, which is the APM-distributed org config;
3. `~/.config/wake/config.json`, which `wake install` writes.

The shape of the file is the same in 2 and 3. If no endpoint resolves, `emit` does nothing.

Performance budget: each `emit` under 50 ms of CPU and under 10 MB RSS, and its work grows with the size of the
current turn, not with the size of the session.

### 5.4 Claude Code adapter: which hooks, and what each does

| Hook | Work |
| --- | --- |
| `SessionStart` | Emit one `wake.inventory` event (§5.6). |
| `Stop` | Read the main transcript from the stored offset to the end. For each subagent that finished in the slice (a foreground `Agent` tool_result, or the `<task-notification>` of a background one), also parse that subagent's transcript file, which is complete by then. Derive and send events (§5.5). Store the new offset, the open invocations and the still-running background subagents. |
| `SessionEnd` | Process any remaining slice, as `Stop` does, then close every open invocation and every still-running background subagent. Keep the state file (see below). |

There is no `SubagentStop` hook. A foreground subagent's summary and transcript are both available when the
parent's `Stop` sees the `Agent` tool_result. A background subagent is held open in the session state until its
`<task-notification>` appears in a later slice. Parent linking is trivial in both cases, because the `Agent`
tool_use is in the parent transcript.

There is no `StopFailure` hook either. A turn that ends in an API error is read at the next `Stop`.

All hooks are `"async": true`. This adds one process per turn, not one per tool call. Hooks say *when* to read. The
current transcript slice says *what* happened. This replaces v1's full rescans with an incremental read of one turn.

**Concurrency.** Each run takes an exclusive, non-blocking lock on `sessions/<session_id>.lock`. If the lock is
held, the run exits. The next `Stop` or `SessionEnd` picks the slice up, because offsets only move forward.

**State.** The binary keeps two kinds of state in `~/.local/state/wake`, or `$XDG_STATE_HOME/wake` (APM provides
no plugin data dir; §3 spike):

- `sessions/<session_id>.json` holds:
  - the byte offset;
  - the open invocations (§5.5), as span id, kind, name, start time and running aggregates;
  - the background subagents still running;
  - the resolved user and repo, cached so `git` runs once per session rather than on every `Stop`.

  The file survives `SessionEnd`, so `--resume` continues from the same offset instead of rescanning. A session that
  never gets `SessionEnd` (killed terminal, crash) would otherwise lose its open invocations, so any run first closes
  and sends the open invocations of other sessions idle for more than 30 minutes, keeping their offsets. Files
  untouched for 14 days are deleted on the next run. A fork (`SessionStart` `source=fork`) starts with an offset at the end of
  the forked transcript, so the copied history is never re-sent.
- `outbox.ndjson`: events whose send failed (§5.8).

There is no other state.

### 5.5 Deriving invocations from a turn slice

The harness's own attribution fields decide which skill a request belongs to. Wake only decides where one
invocation starts and ends.

- **Skill invocation.**
  - **Start:** a `Skill` tool_use (`input.skill`), or a `<command-name>` entry that names a skill.
  - **Belonging:** every later assistant entry whose `attributionSkill` equals that name belongs to that invocation.
    This holds across turns, so a multi-turn skill (brainstorming, `run-sdlc`) is one invocation.
  - **End:** the invocation closes at whichever comes first:
    - the next start of the same skill;
    - 30 minutes with no entry attributed to it;
    - `SessionEnd`.

    It is sent when it closes. A skill that is still open at the end of a turn stays in the session state file.
  - **Aggregates,** over the entries that belong to it:
    - tokens, summed once per `requestId`;
    - `window.duration_ms`, from start to last attributed entry;
    - `window.tool_calls` and `window.tool_errors` (tool_results with `is_error` whose tool_use was attributed to
      the skill);
    - `window.tools`, as tool names with counts, e.g. `Bash:4,Read:2`. Names only.
  - **Nesting:** if a skill starts while another skill's invocation is open and the new start entry is attributed to
    the outer skill, the new invocation's parent is the outer one, and its invoker is `nested`.
- **Subagent invocation.**
  - Its name is the `Agent` tool_use's `input.subagent_type`. It starts at that tool_use.
  - **Foreground:** built at `Stop` from the `Agent` tool_result. `status`, `usage`, `totalToolUseCount`, `toolStats`
    and `totalDurationMs` give the event.
  - **Background** (`async_launched`): kept open in the session state, never reported as an error for being async.
    It closes at the `<task-notification>` with the same tool-use id (`completed` → ok, `killed` → interrupted,
    `failed` → error), or at `SessionEnd` as interrupted. Its tokens and tool count are summed from its own
    transcript, deduplicated by `requestId`.
  - The parent is the skill invocation the `Agent` tool_use was attributed to (`attributionSkill`), if there is one.
  - The subagent's transcript is then parsed with the same rules. Skills, MCP tools and nested subagents inside it
    produce their own events, parented to the subagent. Their tokens are counted there only, never twice.
- **MCP tools.** Aggregated, not one event per call. One `mcp_tool` event per turn per (MCP server, tool, enclosing
  invocation or none), parented to the enclosing invocation, carries the fields below. A skill that spans several
  turns therefore gets one aggregate per turn per MCP tool; the backend sums them under the parent. This keeps MCP
  aggregates out of the state file.
  - `calls`, `errors`;
  - total and max `duration_ms`, from the tool_use and tool_result timestamps.

  Playwright and Atlassian-style MCPs make hundreds of calls per session, and one span per call is the noise §2
  rules out.
- **Slash command that is not a skill:** one `command` event.
- **Invoker** is one of:
  - `user`: the start was a `<command-name>` entry;
  - `model`: the start was a `Skill` tool_use the model chose itself;
  - `nested`: see above.
- **Outcome** for skills and subagents (my call; tool error counts are always sent separately):
  - `error`: the skill failed to load (the `Skill` tool_result has `is_error`), or the subagent's `status` is not
    completed;
  - `interrupted`: the turn ended by user interrupt while the invocation was open, or a background subagent was
    killed or still running at `SessionEnd`. (An API error that ends a turn is not tracked; see §5.4.)
  - `ok`: anything else.

  "Did the skill achieve its goal" is not knowable from telemetry. That belongs to evals.
- **Cost:** tokens are counted where the harness attributed them. A skill's own event carries only its
  main-thread tokens. A subagent it launched carries its own tokens and points to the skill as its parent, and the
  backend rolls up the tree.

### 5.6 The event: what leaves the machine

Events are sent as **OTLP spans** over HTTP JSON, because Langfuse ingests traces.

Langfuse mapping (the builder confirms these against the current Langfuse OTel docs):
- Set `user.id` (the email or the hash) and `session.id` as well, so Langfuse's own user and session views work.
- Emit token counts and the model with the `gen_ai.*` semantic conventions (`gen_ai.request.model`,
  `gen_ai.usage.input_tokens`, `gen_ai.usage.output_tokens`, and the cache fields) on the invocation span, so
  Langfuse prices them. That is how cost in USD is computed without a price table in the binary.
- `wake.*` attributes ride along as metadata for filtering.

- The trace id is a hash of `session_id`, and the span id is a hash of the source `tool_use_id`, or of
  `agent_id` / `prompt_id` for events without one. Retries are therefore idempotent.
- A nested invocation's parent span is its enclosing skill window.

| Attribute | Source |
| --- | --- |
| `wake.harness`, `wake.harness.version` | adapter; transcript `version` |
| `wake.primitive.kind` | skill, subagent, mcp_tool, command |
| `wake.primitive.name` | `input.skill`, `subagent_type`, MCP tool name, command name |
| `wake.primitive.version`, `wake.plugin`, `wake.marketplace` | the plugin manifest on disk that provides the primitive. Empty if it is local. |
| `wake.primitive.context_tokens` | estimated from the size of the primitive's `SKILL.md` / agent file at invocation time |
| `wake.invoker` | user, model, nested |
| `wake.outcome` | ok, error, interrupted |
| `wake.window.duration_ms`, `.tool_calls`, `.tool_errors`, `.tools` | §5.5 |
| `wake.tokens.input`, `.output`, `.cache_read`, `.cache_creation`, `wake.model` | transcript usage. Absent, never zero, when unknown. |
| `langfuse.observation.type` | `generation` on spans that carry a model or tokens, so Langfuse prices them (an explicit type always wins over inference) |
| `wake.session.id` | hook payload |
| `wake.user.email` or `wake.user.hash` | §5.7 |
| `wake.repo` | `git remote get-url origin`, normalised to `host/group/name`. Never a local path. |

**Inventory event.** `SessionStart` sends one `wake.inventory` span, every session, with no local dedup state. The
backend deduplicates per user per day. It carries `wake.inventory` as an array of `kind:name@version` entries
(`skill:superpowers:brainstorming@7.0.2`, with `-` for an unknown version; the plugin is already the name's prefix,
and each entry must pass the allowlist), read from the harness's skill, agent and plugin folders, plus user and harness attributes. Installs is the
count of distinct users who have the primitive in their inventory over the last 30 days. "Installed but never used"
is a backend query, installs minus reach, with no code in the binary.

**Allowlist.** The event is a fixed Rust struct. Every string is checked against a bounded token pattern (letters,
digits and `._:@+_-`; no spaces, no `/` except in `wake.repo`, length-capped). `_` is required: MCP names are
`mcp__<server>__<tool>`. A value that fails the check is dropped, never truncated or guessed.

### 5.7 Identity

- `identity: "email"` (the default) sends `wake.user.email`. The binary takes it from the harness login
  (`~/.claude.json` `oauthAccount.emailAddress`). If there is none, it uses `git config user.email`. If that is also
  missing, the attribute is absent.
- `identity: "anonymous"` sends `wake.user.hash` = HMAC-SHA256(email, org salt from `telemetry.json`), and the email
  never leaves the machine. This is pseudonymisation: anyone with the salt and the staff list can reverse it. Owners
  can still count people without seeing who they are.

### 5.8 Configuration and delivery

- The config file (`bin/telemetry.json` in an APM package, or `~/.config/wake/config.json` from `wake install`;
  resolution order in §5.3) has this shape:

  ```json
  { "endpoint": "https://<langfuse host>/api/public/otel/v1/traces",
    "headers": {"Authorization": "Basic <base64 of public_key:secret_key>"},
    "identity": "email", "identity_salt": "<org secret>", "allow_override": true }
  ```

  JSON rather than TOML, to reuse `serde_json` and avoid an extra crate. This is the builder's call.
- Machine overrides apply when `allow_override` is true: `WAKE_OTLP_ENDPOINT`, `WAKE_OTLP_HEADERS` (OTel `k=v,k=v`
  format), `WAKE_IDENTITY`.
  - The generic `OTEL_EXPORTER_OTLP_*` variables are **deliberately ignored**. Other tools on the machine use them,
    and following them would silently redirect wake.
  - `allow_override=false` is a policy, not a security guarantee: the developer owns the machine.
- **The endpoint is Langfuse directly (my call).** Langfuse's OTLP auth is a public:secret key pair, and the
  secret also gives read access to that Langfuse project through the API. The package is internal to one
  organisation, so this is accepted under three conditions:
  - a **dedicated Langfuse project** used only for wake, so the keys reach nothing else;
  - the **package repo is private**;
  - **rotation** means issuing new keys and releasing a new package version.

  Anyone who has the package can read the project's telemetry, which is the same team that sees the dashboards.
  If that stops being acceptable, put an OTel Collector in front with a write-only token. That change is config
  only.
- Delivery:
  - POST with a 2 s timeout.
  - On failure, append to `outbox.ndjson`. The next `emit` retries the outbox first.
  - The outbox is capped at 1,000 events or 7 days. Beyond that, the oldest events are dropped.
  - No retries inside a single process, no background daemon.
- **Duplicates (my call):** the binary tries to send each event once, using the session lock, forward-only offsets,
  state that survives resume, and fork handling. The one remaining source of duplicates is an outbox resend after a
  timeout that the server did receive, and that is rare. Span ids are deterministic, and Langfuse upserts
  observations by id, so a resent span overwrites itself instead of counting twice. The builder confirms this
  behaviour.

### 5.9 Langfuse setup (in the repo)

- A short setup note: create the dedicated project, generate the keys, and fill in `telemetry.json`.
- Saved Langfuse dashboards for usage, reach, cost, latency, errors, versions, agent and context.
- Retention, co-use, tools breakdown and installs may not be expressible in Langfuse's dashboard widgets. If so,
  they come from a small script against the Langfuse public API (for example a weekly report), kept in this repo.
  The builder checks what the dashboards can do before writing the script.

### 5.10 Later: an organisation on Prometheus and Grafana (deferred)

This is not built in v2.0. It is written up when the first client that needs it arrives. Their copy of the package
points at their OTel Collector or Grafana Alloy, which sends spans to Tempo and derives Prometheus metrics with
`spanmetrics`. The binary does not change. The deployment has three known gaps against Langfuse:

1. **Cost in USD.** Prometheus does not price tokens, so a price table has to live in the collector or in recording
   rules, and someone has to maintain it.
2. **Reach, retention and co-use** need user and session as dimensions. These are too high-cardinality for
   Prometheus labels, so they come from Tempo (TraceQL metrics) or Loki. Usage, latency, errors, cost, versions and
   agent fit Prometheus.
3. **Duplicates.** `spanmetrics` counts a resent span twice, where Langfuse upserts it. Resends are rare, so the
   error is small but not zero.

### 5.11 Later adapters

Pi and Codex each get a thin shim that maps their own event or extension mechanism onto `wake emit --harness <h>`
with the same canonical payload. They are added only if the mechanism exists and the shim stays small. Cost is
absent where the harness does not record tokens per message.

## 6. Metric coverage

| Metric | Derived from |
| --- | --- |
| usage | count of invocation spans per primitive |
| reach | distinct `wake.user.*` and `wake.repo` per primitive |
| retention | users using the primitive in consecutive weeks |
| co-use | primitives sharing a `wake.session.id` |
| cost | `gen_ai.usage.*` + model, priced by Langfuse |
| context | `wake.primitive.context_tokens` |
| latency | `wake.window.duration_ms` |
| errors | `wake.window.tool_errors`, `wake.outcome` |
| tools | `wake.window.tools` |
| versions | `wake.primitive.version` |
| agent | `wake.harness` |
| installs | `wake.inventory` distinct users, 30 days |

## 7. Testing

- Golden tests: sanitised fixture transcripts produce an expected list of events. Fixtures must cover:
  - nested skills;
  - a slash-invoked skill;
  - an interrupted turn;
  - repeated `requestId` usage;
  - a subagent inside a skill;
  - a background subagent that finishes in a later turn;
  - MCP errors.
- Allowlist tests: hostile values (paths, newlines, secrets-shaped strings) never reach a span.
- End-to-end: `wake check` against a local OTLP receiver and against a test Langfuse project; one real Claude Code session with the package installed, and
  the spans visible in Langfuse.
- The performance budget (§5.3) is checked in CI on a large fixture transcript.

## 8. Left to the builder

- **Verify first:**
  - the installed directory name when the package comes from the GitLab marketplace (`apm install
    smos-telemetry@smos -g`). Nothing in the binary depends on it, but the docs and support notes do;
  - the Pi and Codex extension points, before their adapters are planned.

  APM distribution of binaries and hooks was verified by the spike (§3). Never download binaries at runtime.
- **Existing skill telemetry stays as it is.** `smos-infra` ships its own hook (`hooks/supermodular-os-telemetry.sh`)
  that logs skill name and email to a Notion database. v2 does not touch, replace or depend on it. The two write to
  different backends, so neither one's counts include the other's.
- Crate choices (expected: `serde_json`, `ureq` + `rustls`, `hmac`/`sha2`).
- The exact span names, and how `wake.window.tools` and `wake.inventory` are encoded within OTLP attribute limits.
- How the launcher handles an unknown platform: exit 0 silently.
- CI cross-compilation setup and release tagging that fits the marketplace's `v{version}` pattern.
- How v1 is retired in the source repo: tag the last v1, then remove the Go tree on the v2 branch.
