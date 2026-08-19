# wake

[![CI](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/SupermodularAI/agents-wake/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/SupermodularAI/agents-wake?display_name=tag&sort=semver)](https://github.com/SupermodularAI/agents-wake/releases)
[![License](https://img.shields.io/github/license/SupermodularAI/agents-wake)](LICENSE)

Wake is a local CLI for understanding which agent primitives a developer uses,
which ones fail, and which ones are never used. It currently supports Claude
Code and collects only from projects the developer explicitly enables.

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
make build
mkdir -p ~/.local/bin
install -m 0755 dist/wake ~/.local/bin/wake
```

## Use

From the root of a project you want Wake to observe:

```sh
wake init
wake report
```

`wake init` explains what it will change before doing so. It consents the
current project, imports its existing Claude Code history, and installs
Wake-owned Claude Code session hooks. Existing hooks are preserved.

Open the local dashboard when you want a browser view:

```sh
wake serve
```

Wake listens only on [127.0.0.1:8080](http://127.0.0.1:8080). The hooks keep
collection current; use these commands when needed:

```sh
wake ingest             # Re-scan consented history
wake ingest --rebuild   # Rebuild the derived event store
wake doctor             # Inspect collection and hook health
wake remove             # Remove the Claude Code integration; keep local data
wake remove --purge     # ...and delete collected data; ~/.config/wake is kept
wake uninstall          # Remove everything, including ~/.config/wake and the binary
```

| Command | Purpose |
| --- | --- |
| `wake report` | Print local activity in the terminal. |
| `wake serve` | Open the local dashboard. |
| `wake init` | Enable collection for the current project. |
| `wake ingest` | Import activity for consented projects. |
| `wake doctor` | Show collection and hook health. |
| `wake remove` | Remove Wake-owned Claude Code hooks. `--purge` also deletes collected data; `~/.config/wake` is kept either way, so a later `wake init` keeps the same repository identity. |
| `wake uninstall` | Irreversible. Removes the integration, all collected data, `~/.config/wake` (configuration and the identity salt) and the binary itself. It prints every path before deleting anything. |

## Privacy And Enterprise Use

Wake is designed for enterprise environments where agent transcripts may
contain source code, credentials, or customer data. It is not a compliance
certification; organizations should still apply their own security and policy
review. Its default design keeps the sensitive path local:

- Collection requires explicit, per-project consent. A fresh install collects
  nothing.
- Wake reads Claude Code transcripts to derive measurements, but it never
  persists prompts, tool arguments, code, repository paths, or repository
  labels in its event records.
- Records are structurally limited to identifiers, hashes, timestamps, enums,
  and counters. Invalid or path-shaped values are dropped rather than stored.
- Repository identity is a salted, per-machine HMAC. The readable project map
  stays local with restrictive permissions.
- The official binaries contain no remote-delivery code. Wake sends no
  transcript data or telemetry, and the dashboard is bound to loopback only.
- `wake doctor` reports safe counters, never transcript content or repository
  paths, so its output is suitable for sharing in support requests.

This means security teams can inspect a small local data boundary: the source
transcript stays with its harness, Wake stores only derived metadata, and no
network destination is configured by default.

## Local State

Wake stores configuration under `~/.config/wake` and derived state under
`~/.local/state/wake`. Set `WAKE_DIR` to move the state directory. Removing the
derived event store is safe: `wake ingest --rebuild` recreates it from consented
history.

## Development

```sh
make validate
make test-race
```

## License

[MIT](LICENSE)
