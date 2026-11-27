# Security Policy

## Supported Versions

Wake is pre-1.0. Security fixes land on `main` and ship in the next tagged
release; only the latest release is supported.

| Version | Supported |
| --- | --- |
| Latest release | Yes |
| Anything older | No — upgrade first |

## Reporting a Vulnerability

Report privately. **Do not open a public issue for a security report.**

Email [contact@supermodular.ai](mailto:contact@supermodular.ai). If GitHub's
private vulnerability reporting is enabled on this repository, Security →
Report a vulnerability works too.

Please include the version (`wake --version`), your platform, what an attacker
gains, and the smallest reproduction you have. If you have a suggested fix,
say so — it usually shortens the turnaround.

What to expect:

- An acknowledgement that the report arrived, and an assessment with our
  severity reading once we have one. Wake is a small project; we aim to reply
  promptly rather than to a fixed schedule.
- Credit in the release notes when a report leads to a fix, unless you prefer
  otherwise.

Please give us a reasonable window to ship a fix before disclosing publicly.

**Never include a real credential in a report.** If a live Langfuse key or
collector token appears in a transcript, log, or screenshot you are about to
attach, rotate it first and redact it in the attachment.

## What Wake Touches

Wake is a small binary that Claude Code runs asynchronously from its
`SessionStart`, `Stop` and `SessionEnd` hooks. It reads the new part of the
session transcript, derives which skills, subagents, MCP tools and commands
ran, and sends one OTLP/HTTP JSON span per invocation to the endpoint in its
config. It reads transcripts that may contain source code, credentials or
customer data, so the boundary that matters most is what it lets out.

### What leaves the machine

Only the spans described in the design spec (§5.6), to the configured
endpoint. Each span is a fixed Rust struct, and every string in it must pass a
bounded token allowlist (letters, digits and `._:@+_-`, length-capped); a value
that fails is dropped, never truncated or guessed. A span carries:

- the harness and its version, the session id, and the trace and span ids
  (hashes of the session id and of the source `tool_use_id`/agent id);
- the primitive's kind, name, version, plugin and marketplace, and its
  estimated context size;
- invoker, outcome, duration, tool-call and tool-error counts, and the names of
  the tools it used;
- the model and the token counts from the transcript's usage records;
- **`wake.repo`**: the `origin` remote normalised to `host/group/name`, sent
  **in clear**. It is never a local path;
- **the user**: `wake.user.email` (the Claude Code login, else
  `git config user.email`) in `email` mode, or in `anonymous` mode
  `wake.user.hash`, an HMAC-SHA256 of the email with the organisation's
  `identity_salt`. That is pseudonymisation, not anonymity: anyone holding the
  salt and a staff list can reverse it. Without a salt, anonymous mode sends no
  user at all;
- on `SessionStart`, an inventory of installed skills, agents and plugins as
  `kind:name@version` entries.

Prompt text, completions, tool arguments and results, file contents and local
paths are never sent. In scope: any of that reaching a payload (including
through a malformed or hostile transcript), any string that bypasses the
allowlist, any network egress other than the configured endpoint, and any send
while `WAKE_DISABLED=1` is set or no endpoint is configured. `wake.repo` and,
in `email` mode, the email are sent by design and are not findings.

### What stays local

- **`~/.config/wake/config.json`** (or `$XDG_CONFIG_HOME/wake/config.json`),
  written by `wake install` with mode `0600` because it holds the endpoint's
  credentials. An APM package instead ships its org config as
  `bin/telemetry.json` next to the binary.
- **`~/.local/state/wake`** (or `$XDG_STATE_HOME/wake`), mode `0700`: one
  state file per session (transcript offset, open invocations, the cached user
  and repo, the transcript path) and **`outbox.ndjson`** (mode `0600`), which
  holds encoded spans whose send failed, until they are delivered, rejected
  for good, or pruned (1,000 entries or 7 days). Queued spans hold exactly what
  would have been sent, nothing more. `wake uninstall --purge` deletes both
  directories.

Wake reads prompt text only transiently, while parsing a transcript line; it
is never kept in the derived entries, the state files or the outbox. In scope:
any transcript content landing in these files, and file modes that expose them
to other unprivileged local users.

### Install and support commands

- `wake install` merges its three hook groups into `~/.claude/settings.json`
  and recognises its own groups only by an exact command match (the installed
  binary's path followed by `emit <event>`). `wake uninstall` removes exactly
  those groups and leaves the file otherwise byte-identical. Neither ever
  touches hooks installed by APM or any other tool, and both refuse to write
  when `settings.json` cannot be parsed. A way to make either command modify
  or remove a hook it does not own is in scope.
- `wake check` prints the endpoint's origin only (never its path or query),
  the number of headers set (never their values), the identity mode (never the
  identity itself), and the source of each value. Anything that makes `check`,
  an error message or `emit` print a secret is in scope. `emit` never writes
  to stdout or stderr.
- `install.sh` and the released binaries: anything that lets a third party
  alter what a user installs (checksums are verified against the release).

### The Langfuse key caveat

When the endpoint is Langfuse directly, the `Authorization` header is a
public:secret key pair, and the secret key also grants **read** access to that
Langfuse project through its API. Anyone who can read the config (or the
installed package's `telemetry.json`) can read that project's telemetry. This
is accepted by design (spec §5.8) under three conditions: a dedicated Langfuse
project used only for wake, a private package repository, and rotation by
issuing new keys and releasing a new package version. If that is not
acceptable, put an OTel Collector with a write-only token in front; that is a
config change only. Reports that merely restate this caveat are out of scope.

### Out of scope

- **The endpoint you configure.** The collector is your trust boundary, not
  Wake's. Vulnerabilities in Langfuse or any other OTLP/HTTP receiver belong
  to that project. Wake sending the wrong thing or to the wrong place is in
  scope.
- **Content in the transcripts themselves.** Wake reads what its harness wrote.
  A credential pasted into a prompt is a matter for that harness and that
  developer; Wake's obligation is not to persist or transmit it.
- **An attacker who is already root, or who can already read the user's home
  directory.** Such an attacker can read the transcripts directly.
- `allow_override=false` is a policy, not a security control: the developer
  owns the machine and can always change the environment.
- Findings from an automated scanner with no demonstrated impact. Please
  include a reproduction.

## Design Notes For Reviewers

- **Span ids are deterministic** (hashes of session and tool-use ids), so an
  outbox resend after a timeout the server did receive overwrites the same
  observation instead of counting twice.
- **Delivery is bounded.** A run flushes at most 50 queued payloads per POST,
  starts no POST after 5 s, and uses a 2 s per-request timeout. There are no
  retries inside a process and no background daemon.
- The generic `OTEL_EXPORTER_OTLP_*` variables are deliberately ignored, so
  another tool's settings cannot redirect Wake. `WAKE_OTLP_HEADERS` in the
  environment overrides the stored headers when `allow_override` is true; an
  environment variable is readable by other processes running as the same user.
