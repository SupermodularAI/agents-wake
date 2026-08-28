# wake

[![CI](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SupermodularAI/agents-wake?display_name=tag&sort=semver)](https://github.com/SupermodularAI/agents-wake/releases)
[![License](https://img.shields.io/github/license/SupermodularAI/agents-wake?cacheSeconds=3600)](LICENSE)

Wake is a local CLI for understanding which agent primitives a developer uses,
which ones fail, and which ones are never used. It currently supports Claude
Code and collects only from projects the developer explicitly enables.

![wake report](docs/wake_report.gif)

## Install

Official binaries are available for macOS and Linux on amd64 and arm64:

```sh
curl -fsSL https://raw.githubusercontent.com/SupermodularAI/agents-wake/main/install.sh | sh
```

The installer downloads the latest release, verifies its SHA-256 checksum, and
installs `wake` in `~/.local/bin` without `sudo`. It prints a PATH reminder when
needed. Releases also include an SBOM and GitHub build provenance.

To verify the installed binary's provenance with the GitHub CLI:

```sh
gh attestation verify ~/.local/bin/wake --repo SupermodularAI/agents-wake
```

To build from source, use Go 1.25 or newer:

```sh
git clone https://github.com/SupermodularAI/agents-wake.git
cd agents-wake
make install
```

`make install` builds and installs to `~/.local/bin` — the same place and
`WAKE_INSTALL_DIR` override the shell installer above uses — and tells you if
that directory isn't on your `PATH`. `make build` alone only builds
`dist/wake`; it isn't on `PATH`, so running `wake` from outside the repo
without `make install` (or `go install ./cmd/wake`, which puts it in
`$(go env GOPATH)/bin` instead) fails with a plain "command not found".

## Use

From the root of a project you want Wake to observe:

```sh
wake init
wake report
```

`wake report` prints usage for the primitives you've used, one row per
primitive, with a REPO column naming the repository it was used in — the
readable label from your local project map, never the path. The unused list
has no such column: a repository is a property of an invocation, and an unused
primitive has none. Add `--unused` to see primitives that are available but
never invoked, or both flags together for the full picture. A bare `--unused`
swaps the OVERVIEW too — invocation counts and outcomes describe activity, so
the overview above an unused list is instead a count of unused primitives by
kind. In a terminal the tables are lime-bordered and colored; piped or
redirected — a script, another program, an agent reading the output — it's
plain ASCII text instead, so nothing downstream ever has to parse around a
color code.

`wake init` explains what it will change before doing so. It consents the
current project and installs Wake-owned Claude Code session hooks, and
collection starts from that moment. Existing hooks are preserved.

On a terminal, `init`, `ingest`, `remove`, `uninstall`, `report` and `serve`
all show a lime spinner while their real work runs — importing history,
rebuilding the event store, refreshing the primitive inventory, removing the
integration — and confirm with a lime checkmark. Piped or redirected, every
one of them is the same plain, deterministic text instead.

Existing history is not imported by default, and nothing imports it later on
its own: the session hooks collect only what happens after `wake init`. When
you want the history, ask for it — `wake init --full` imports this project's
Claude Code history in the same call, and `wake ingest` does it afterwards for
a project that is already consented.

If you'd rather consent a whole tree once than run `wake init` in every
project, `wake init -g ~/Developer` consents everything under a directory;
`wake init -g` on its own means your home directory. Each repository under the
boundary is registered under **its own** identity the first time a scan sees a
session in it — including repositories you create later — so `report` and
`serve` keep their per-project breakdown rather than folding the tree into one.
Each one starts collecting from the moment it is registered, the same
forward-only default a plain `wake init` gives, and `wake init -g --full`
imports the existing history under the boundary in the same call. `wake doctor`
says whether a boundary is set, and counts the consented repositories it
encloses — including any you consented with a plain `wake init` before the
boundary existed, so the number is what is under the boundary rather than what
the boundary found.

Open the local dashboard when you want a browser view:

```sh
wake serve
```

Wake listens only on [127.0.0.1:8080](http://127.0.0.1:8080). The hooks keep
collection current; use these commands when needed:

```sh
wake init --full         # Consent and import existing history now
wake init -g ~/Developer # Consent every project under a directory
wake ingest              # Re-scan consented history
wake ingest --rebuild    # Rebuild the derived event store
wake doctor              # Inspect collection and hook health
wake update              # Install the newest release, verifying its checksum
wake update --check      # Report whether a newer release exists; download nothing
wake remove              # Remove the Claude Code integration; keep local data
wake remove --purge      # ...and delete collected data; ~/.config/wake is kept
wake uninstall           # Remove everything, including ~/.config/wake and the binary
```

| Command | Purpose |
| --- | --- |
| `wake report` | Print local activity in the terminal: the primitives you've used. |
| `wake report --unused` | ...swap in the primitives available but never invoked. |
| `wake report --usage --unused` | ...both sections. |
| `wake serve` | Open the local dashboard. |
| `wake init` | Enable collection for the current project. |
| `wake init --full` | Enable collection and import existing history now. |
| `wake init --global [path]` | Consent every project under a directory (your home directory when no path is given), registering each repository under its own identity as sessions run in it. Records the boundary; consents no root of its own. |
| `wake init --global --full` | ...and import the existing Claude Code history under that boundary in the same call. |
| `wake ingest` | Import activity for consented projects. |
| `wake doctor` | Show collection and hook health. |
| `wake remove` | Remove Wake-owned Claude Code hooks. `--purge` also deletes collected data; `~/.config/wake` is kept either way, so a later `wake init` keeps the same repository identity. |
| `wake uninstall` | Irreversible. Removes the integration, all collected data, `~/.config/wake` (configuration and the identity salt) and the binary itself — plus the symlink you invoked it through, if the `wake` on your PATH is a link. It prints every path before deleting anything, and removes nothing at all if it cannot take its hook entry out of `settings.json` first. |
| `wake update` | Download the newest release, verify its SHA-256 against the published `checksums.txt`, and replace this binary in place. Refuses and changes nothing if the checksum does not match. |
| `wake update --check` | Report whether a newer release exists and stop there — it downloads nothing. On a build with no release tag it says so rather than guessing. |
| `wake remote` | Configure and control delivery to a remote endpoint — see [Remote Delivery](#remote-delivery) below. |

## Privacy And Enterprise Use

Wake is designed for enterprise environments where agent transcripts may
contain source code, credentials, or customer data. It is not a compliance
certification; organizations should still apply their own security and policy
review. Its default design keeps the sensitive path local:

- Collection requires explicit, per-project consent. A fresh install collects
  nothing, and consent starts collection from the moment it is given: importing
  a project's existing history is a separate, explicit request.
- Wake reads Claude Code transcripts to derive measurements, but it never
  persists prompts, tool arguments, code, repository paths, or repository
  labels in its event records.
- Records are structurally limited to identifiers, hashes, timestamps, enums,
  and counters. Invalid or path-shaped values are dropped rather than stored.
- Repository identity is a salted, per-machine HMAC. The readable project map
  stays local with restrictive permissions; its labels are what `wake report`
  and the dashboard show in their REPO column. When you turn remote delivery on,
  a payload carries that hash and the repository's readable label; the
  repository path never leaves the machine.
- Every binary ships the remote-delivery capability and it is off until you run
  `wake remote set [url]` and `wake remote on`. Until you do, no endpoint is
  configured, nothing is sent, and `wake remote status` and `wake doctor` both
  say so. The dashboard is bound to loopback only.
- `wake update` and `wake update --check` are the only commands that reach the
  network without being asked to deliver, and only when you run them: nothing
  checks for updates in the background, and both shell out to `curl` rather than
  linking a network client.
- `wake doctor` reports safe counters, never transcript content or repository
  paths, so its output is suitable for sharing in support requests.

This means security teams can inspect a small local data boundary: the source
transcript stays with its harness, Wake stores only derived metadata, and no
network destination is configured by default.

## Remote Delivery

Wake can deliver its derived records to an OTLP/HTTP JSON-compatible
collector, such as [Langfuse](https://cloud.langfuse.com). The capability
ships in every build and stays off until you configure it:

```sh
wake remote set                                                        # at a terminal: prompts for the URL and both keys
wake remote set https://cloud.langfuse.com/api/public/otel/v1/traces   # scripted: credential on stdin as public:secret
wake remote on
wake remote flush
wake remote status
```

| Command | Purpose |
| --- | --- |
| `wake remote set [url]` | Configure the delivery endpoint. At a terminal it prompts for the URL if you did not pass one, shows the destination's bare host and asks you to confirm it, then asks for the public key (shown) and the secret key (not shown). Piped or in CI it is unchanged: the URL is an argument and the joined `public:secret` credential is read whole from standard input, never as an argument. Neither path ever echoes the secret key or the joined credential, and neither ever prints the full URL. |
| `wake remote on` | Start delivering records to the configured endpoint. |
| `wake remote off` | Stop delivering; the endpoint is kept, so nothing needs re-entering to resume. |
| `wake remote flush` | Deliver everything pending now. Add `--dry-run` to print the exact payload the next flush would send, without sending it. |
| `wake remote status` | Report the endpoint's bare host, whether a credential and delivery are configured, the last flush time, and what's pending. This is the one command allowed to name the endpoint; `doctor` and everything else report presence only. |

What travels is one span per record: a trace per session, each invocation
parented onto its session, named after the repository so the receiver can group
by it. Spans carry structure, timing, model, tokens and outcome — never prompt
or completion text. Built-in tool calls are counted locally but not sent as
spans of their own. Nothing OTLP-shaped is persisted: the payload is computed
from the local event store at flush time, held for one POST, then discarded,
and every id is derived from `event_id`, so a retried flush deduplicates at the
receiver rather than double-counting.

A `wake ingest` scan triggers a detached flush automatically once delivery is
on, throttled by `remote.min_interval` (15 minutes by default, provisional) —
run `wake remote flush` directly whenever you want one immediately.
`WAKE_REMOTE_AUTHORIZATION` overrides the stored credential when set, useful
for CI or anyone who prefers no secret on disk at all.

## Local State

Wake stores configuration under `~/.config/wake` and derived state under
`~/.local/state/wake`. Set `WAKE_DIR` to move the state directory. Removing the
derived event store is safe: `wake ingest --rebuild` recreates it from consented
history.

The collection boundary `wake init --global` records lives in the project table
under the state directory, beside the repository identities, and never in
`config.toml` — there is no configuration key for it, so it cannot be widened by
editing a settings file.

## Development

```sh
make validate
make test-race
```

## License

[MIT](LICENSE)
