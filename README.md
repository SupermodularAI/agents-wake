# wake

Wake shows the Claude Code primitives used in consented local projects. It
derives terminal tool events from local Claude Code history, stores only
structured measurements, and serves a local dashboard.

This branch is an MVP. It supports Claude Code only.

## Install

### Official binaries

Wake supports macOS and Linux, on amd64 and arm64. There is no Windows build,
and `wake` refuses to start on a platform it does not support rather than
half-working.

Download from the
[Releases page](https://github.com/SupermodularAI/agents-wake/releases). Each
release carries one `tar.gz` per supported platform, a `checksums.txt`, a
per-archive SBOM, and a signed build provenance attestation.

Download, verify, and place the binary. No `sudo`, and nothing is written outside
the target directory:

```sh
tag=v0.1.0
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
archive="wake_${tag#v}_${os}_${arch}.tar.gz"
base="https://github.com/SupermodularAI/agents-wake/releases/download/$tag"
curl -fsSLO "$base/$archive" && curl -fsSLO "$base/checksums.txt"

grep " $archive\$" checksums.txt | shasum -a 256 --check -   # macOS
grep " $archive\$" checksums.txt | sha256sum --check -       # Linux

tar -xzf "$archive" wake
mkdir -p ~/.local/bin && mv wake ~/.local/bin/wake
```

If `~/.local/bin` is not already on your `PATH`, add it:

```sh
export PATH="$HOME/.local/bin:$PATH"
```

Optionally verify the build provenance:

```sh
gh attestation verify ~/.local/bin/wake --repo SupermodularAI/agents-wake
```

Installing places a binary and changes nothing else. Wake writes no harness
configuration until you run `wake init`.

### From source

Building from source requires Go 1.25 or newer.

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

An official binary reports its release tag, short commit, and build date. A
source build reports whatever `git describe` found in your checkout, and
`dev (commit none, built unknown)` when it was built without the Makefile — so
you can always tell which of the two you are running.

## Start Using Wake

From the root of a project with Claude Code history:

```sh
wake init
```

`init` prints the exact local files it will change, then changes them: it
registers the current repository as consented, imports available Claude Code
history, and adds Wake-owned `SessionStart` and `SessionEnd` triggers to the
global Claude Code settings. Existing Claude Code hooks are preserved.

The trigger it writes is the **absolute path** of the `wake` binary that ran
`init`, because `make build` leaves the binary in `dist/` and never on your
`PATH`. Re-run `wake init` after moving or reinstalling the binary — that is
what repoints the hook at the new location. An installation whose path cannot be
used as a hook command is refused before any file is changed, with a message
saying what the path has to look like.

Open the dashboard explicitly:

```sh
wake serve
```

Then open [http://127.0.0.1:8080](http://127.0.0.1:8080). The server binds to
loopback only. `wake serve` binds the port before printing the URL, so the
message appears only once the dashboard is actually listening, and every request
phase is time-bounded.

In a terminal, running `wake` starts the dashboard on the default port. When
stdout is not a terminal, it prints deterministic invocation and session counts
instead of starting a server.

## Commands

| Command | Purpose |
| --- | --- |
| `wake` | Start the local dashboard in a terminal, or print deterministic text otherwise. |
| `wake serve` | Serve the local dashboard on `127.0.0.1:8080`. |
| `wake init` | Consent the current project, install Wake-owned Claude Code triggers, and import history. |
| `wake ingest` | Re-scan Claude Code history for already consented projects. |
| `wake ingest --quiet` | The Claude Code trigger form: scans in a detached background process, prints nothing, and always exits 0. |
| `wake doctor` | Report what the last scan and the last hook change managed to do, as counts. |

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
- A hook only runs `<path to wake> ingest --quiet`; it sends no event payload. The
  history remains the source of truth. The hook says *when* to look; the history
  says *what* was done.
- `wake doctor` reports counts and timestamps only. No path, label, repository ID,
  or line of any transcript reaches it, which is what makes its output safe to
  paste into a bug report.
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
| `~/.local/state/wake/health.json` | Counts from the last scan and the last hook change; safe to delete. |

Set `WAKE_DIR` to move the state directory. It does not move the configuration
directory or repository salt.

## Development

```sh
make validate
```

This runs formatting checks, `go vet`, golangci-lint, and unit tests.
