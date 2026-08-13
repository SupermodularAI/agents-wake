# wake

Wake shows the Claude Code primitives used in consented local projects. It
derives terminal tool events from local Claude Code history, stores only
structured measurements, and serves a local dashboard.

This branch is an MVP. It supports Claude Code only.

## Install

Wake requires Go 1.25 or newer.

```sh
git clone https://github.com/SupermodularAI/agents-wake.git
cd agents-wake
make build
```

`make build` compiles the binary to `dist/wake`; it does not install `wake` on
your `PATH`. Use it directly from the checkout:

```sh
./dist/wake init
./dist/wake serve
```

To install the version in the current checkout as `wake`:

```sh
go install ./cmd/wake
```

`go install` places the binary in `$(go env GOPATH)/bin`. Add that directory to
your `PATH` before running `wake` without a path prefix:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Add that line to your shell profile, such as `~/.zshrc`, to keep it available in
new terminals. Confirm the installation with:

```sh
wake --version
```

## Start Using Wake

From the root of a project with Claude Code history, preview the local files
Wake will change:

```sh
wake init
```

Enable collection after reviewing the paths:

```sh
wake init
```

`init` registers the current repository as consented, imports available Claude
Code history, and adds Wake-owned `SessionStart` and `SessionEnd` triggers to
the global Claude Code settings. Existing Claude Code hooks are preserved.

Open the dashboard explicitly:

```sh
wake serve
```

Then open [http://127.0.0.1:8080](http://127.0.0.1:8080). The server binds to
loopback only.

In a terminal, running `wake` starts the dashboard on the default port. When
stdout is not a terminal, it prints deterministic invocation and session counts
instead of starting a server.

## Commands

| Command | Purpose |
| --- | --- |
| `wake` | Start the local dashboard in a terminal, or print deterministic text otherwise. |
| `wake serve` | Serve the local dashboard on `127.0.0.1:8080`. |
| `wake init` | Consent the current project, install Wake-owned Claude Code triggers, and import history. |
| `wake ingest [--quiet]` | Re-scan Claude Code history for already consented projects. |

## What The Dashboard Shows

- Terminal invocation count.
- Distinct sessions with primitive activity.
- Known outcomes and the count excluded because the source did not report an
  outcome.
- Error rate with its known-outcome denominator.
- Usage, session count, latest activity, and error rate per observed primitive.

When there is no stored history, the dashboard reports `No terminal events yet`.
Harnesses not supported by this MVP are marked as not observed; Wake does not
present them as zero usage.

After upgrading Wake's event derivation, rebuild the derived local event store
without changing consent or hooks:

```sh
wake ingest --rebuild
```

## Data And Privacy

- Wake reads Claude Code JSONL history under `~/.claude/projects/`.
- Only events whose recorded working directory belongs to a project explicitly
  consented through `wake init` can reach the local store.
- A hook only runs `wake ingest --quiet`; it sends no event payload. The history
  remains the source of truth.
- Stored records contain bounded identifiers, hashes, timestamps, enums, and
  numbers. They do not contain prompts, tool arguments, repository paths, or
  repository labels.
- Wake does not send data over the network. `wake serve` is a local loopback
  server only.

## Local Files

| Location | Purpose |
| --- | --- |
| `~/.config/wake/config.toml` | Wake configuration and consented repository IDs. |
| `~/.config/wake/repo-salt` | Per-machine salt used to derive repository IDs. |
| `~/.local/state/wake/projects.json` | Local hashed-ID-to-project map; mode `0600`. |
| `~/.local/state/wake/events.ndjson` | Derived terminal event store. |

Set `WAKE_DIR` to move the state directory. It does not move the configuration
directory or repository salt.

## Development

```sh
make validate
```

This runs formatting checks, `go vet`, golangci-lint, and unit tests.
