# wake

[![CI](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SupermodularAI/agents-wake?display_name=tag&sort=semver)](https://github.com/SupermodularAI/agents-wake/releases)
[![License](https://img.shields.io/github/license/SupermodularAI/agents-wake?cacheSeconds=3600)](LICENSE)

Wake is a small binary that Claude Code hooks call to report which skills, subagents, MCP tools and
commands a developer uses. It sends one OTLP span per use to an endpoint you choose, Langfuse
included, so a team can see reach, cost, latency and errors per primitive.

It runs inside the hooks. There is no daemon, and it never prints or fails: `wake emit` always exits 0.

## What it sends

Only the fields below, and only values that pass a strict token check (letters, digits and `._:@+_-`).
A value that fails the check is dropped, not truncated.

| Attribute | Meaning |
| --- | --- |
| `wake.harness`, `wake.harness.version` | `claude-code` and its version |
| `wake.primitive.kind`, `wake.primitive.name` | skill, subagent, mcp_tool or command, and its name |
| `wake.primitive.version`, `wake.plugin`, `wake.marketplace` | from the plugin that provides it; absent if local |
| `wake.primitive.context_tokens` | estimated size of the skill or agent file |
| `wake.invoker`, `wake.outcome` | user, model or nested; ok, error or interrupted |
| `wake.window.duration_ms`, `.tool_calls`, `.tool_errors`, `.tools` | what happened while the primitive was active |
| `wake.mcp.*` | per-server call, error and duration totals for MCP tools |
| `gen_ai.request.model`, `gen_ai.usage.input_tokens`, `.output_tokens`, `.cache_read_input_tokens`, `.cache_creation_input_tokens` | from the transcript's usage; absent when unknown. Langfuse prices them |
| `langfuse.observation.type` | `generation` on spans that carry a model or tokens |
| `session.id`, `wake.session.id` | the Claude Code session |
| `user.id`, and `wake.user.email` or `wake.user.hash` | your email, or a salted HMAC with `identity: anonymous` |
| `wake.repo` | `host/group/name` of the `origin` remote. Never a local path |
| `wake.inventory` | on `SessionStart`: the installed skills, agents and plugins as `kind:name@version` |

**It never sends** prompt or response text, tool inputs or outputs, Bash commands, file contents, file
paths or local repository paths. It does not read the generic `OTEL_EXPORTER_OTLP_*` variables, on
purpose: other tools use them, and following them would silently redirect wake.

## Install

### Open-source path (one machine)

```sh
curl -fsSL https://raw.githubusercontent.com/SupermodularAI/agents-wake/main/install.sh | sh
wake install
```

`install.sh` downloads the binary for your platform (macOS or Linux, arm64 or x86_64) from the latest
release, verifies its SHA-256 against `checksums.txt`, and puts it in `~/.local/bin`. Set
`WAKE_INSTALL_DIR` to put it elsewhere. It never uses sudo.

`wake install` writes the complete config to `~/.config/wake/config.json` (or
`$XDG_CONFIG_HOME/wake/config.json` when that is set) with mode 0600, and adds its three hooks
(`SessionStart`, `Stop`, `SessionEnd`) to `~/.claude/settings.json`. At a terminal it asks for the endpoint,
public key and secret key; otherwise pass `--endpoint URL`, `--public-key K` and `--secret-key K`, or fill them in
by hand afterwards (see Configuration). It never overwrites a value you already set, and it refuses to touch a
`settings.json` that is not plain JSON. Re-run it any time.

### Organisation path (APM)

An organisation copies [`package/`](package/README.md) into its own private repository, adds its
`telemetry.json` and the release binaries, and developers run:

```sh
apm install <package>@<marketplace> --target claude -g
```

Developers on this path configure their machine with the environment variables below only.

## Check it works

```sh
wake check
```

Prints where the endpoint, auth and identity came from (never keys, header values or the path), then
sends one test event. It exits 0 if the event was delivered. A Langfuse setup walk-through is in
[docs/langfuse-setup.md](docs/langfuse-setup.md).

## Configuration

| Variable | Effect |
| --- | --- |
| `WAKE_OTLP_ENDPOINT` | OTLP/HTTP traces URL. Must be `https://`, or `http://` for localhost |
| `WAKE_PUBLIC_KEY`, `WAKE_SECRET_KEY` | Key pair; wake sends `Authorization: Basic base64(public_key:secret_key)` |
| `WAKE_OTLP_HEADERS` | Other headers as `k=v,k2=v2` (values may be percent-encoded) |
| `WAKE_IDENTITY` | `email` (default) or `anonymous` |
| `WAKE_DISABLED` | `1` or `true`: send nothing |

`wake install` writes this file, and you only fill in the endpoint and the two keys:

```json
{
  "endpoint": "https://<langfuse host>/api/public/otel/v1/traces",
  "public_key": "pk-lf-...",
  "secret_key": "sk-lf-...",
  "headers": {},
  "identity": "email",
  "identity_salt": "",
  "allow_override": true
}
```

Wake builds the Basic auth header from the two keys. `headers` is for other backends, and an `Authorization`
header set there wins over the keys. Empty strings count as unset.

Resolution order, per setting: these variables, then `telemetry.json` next to the binary, then
`~/.config/wake/config.json` (`$XDG_CONFIG_HOME/wake/config.json` when `XDG_CONFIG_HOME` is set). A
config file with `"allow_override": false` ignores every variable except `WAKE_DISABLED`, which is always
honoured.

Anonymous mode needs an `identity_salt`, an organisation secret set in the config file (there is no
variable for it): `{"identity": "anonymous", "identity_salt": "<org secret>"}`. `wake.user.hash` is
HMAC-SHA256 of your email with that salt. Without a salt, anonymous mode sends no user at all rather than
an unsalted hash. `wake install` keeps an `identity_salt` you put in its config file.

If a send fails, the event is queued in `$XDG_STATE_HOME/wake` (default `~/.local/state/wake`) and retried by
the next `wake emit`. The queue holds at most 1,000 events or 7 days.

### Turning it off

`WAKE_DISABLED=1` works at four levels:

- **One command:** `WAKE_DISABLED=1 claude`
- **Every session:** `export WAKE_DISABLED=1` in your shell profile
- **One user, any shell:** in the `env` block of `~/.claude/settings.json`: `{"env": {"WAKE_DISABLED": "1"}}`
- **One repository** (client work, for example): the same `env` block in that repository's
  `.claude/settings.local.json`, which Claude Code passes to the hooks of sessions started there

## Uninstall

```sh
wake uninstall            # removes wake's hooks from ~/.claude/settings.json
wake uninstall --purge    # also removes ~/.config/wake and the state directory
```

The binary stays where it is; delete it yourself. Hooks installed by APM are not touched by
`wake uninstall`; remove them with `apm uninstall -g <package>`.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md). Security reports: [SECURITY.md](SECURITY.md). MIT licensed.
