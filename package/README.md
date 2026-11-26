# wake APM package template

A template for distributing [wake](../README.md) through [APM](https://github.com/microsoft/apm)
inside an organisation. It holds no code and no secrets.

## Use it

1. Copy this directory into a **private** repository owned by your organisation. `telemetry.json` will
   carry Langfuse keys, so the repository must not be public.
2. Rename the package in `apm.yml` and `.claude-plugin/plugin.json`.
3. Copy `bin/telemetry.example.json` to `bin/telemetry.json` and fill it in. See
   [docs/langfuse-setup.md](../docs/langfuse-setup.md) for the keys and the endpoint.
4. Download the four binaries from a [wake release](https://github.com/SupermodularAI/agents-wake/releases)
   into `bin/`, check them against `checksums.txt`, and keep these names:
   `wake-darwin-arm64`, `wake-darwin-x64`, `wake-linux-arm64`, `wake-linux-x64`.
5. Make the launcher and the binaries executable, and commit them with the exec bit (git mode `100755`):

   ```sh
   chmod +x bin/wake bin/wake-*
   git add bin/wake bin/wake-*
   git update-index --chmod=+x bin/wake bin/wake-*
   git ls-files -s bin   # wake and wake-* must show 100755
   ```

   This matters because the launcher fails safe: if the binary it picks is not executable, it exits 0 without
   doing anything, so a package without the exec bit installs cleanly and then silently sends nothing.
6. Publish the package through your APM marketplace. Developers install it with:

   ```sh
   apm install <package>@<marketplace> --target claude -g
   ```

## Why user scope only

Always install with `-g`. APM writes the hooks into `~/.claude/settings.json` and rewrites `${PLUGIN_ROOT}` to
`$HOME/.claude/hooks/<package>/`. A project-scope install would put the path in a repository's settings, where it
does not exist for anyone else.

## How it fits together

- `bin/wake` is a POSIX sh launcher. It picks the binary by `uname -s` and `uname -m`, and exits 0 quietly on an
  unknown platform or a missing binary, so a hook never fails.
- APM copies only the directory a hook command points at, so `telemetry.json` has to live in `bin/`, next to the
  binary. The binary reads it from its own directory.
- Developers configure their own machine with environment variables only (`WAKE_OTLP_ENDPOINT`,
  `WAKE_OTLP_HEADERS`, `WAKE_IDENTITY`, `WAKE_DISABLED`). Editing the installed `telemetry.json` is unsupported:
  the next APM update overwrites it. Set `"allow_override": false` to make the file win over those variables.
- Rotating the Langfuse keys means issuing new keys and releasing a new package version.

## Uninstall

```sh
apm uninstall -g <package>
```
