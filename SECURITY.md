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

Wake's security surface is narrow but specific: it reads agent transcripts that
may contain source code, credentials, or customer data; it stores derived
metadata locally; and it can be configured to deliver that metadata to a
collector you own. Reports that concern these boundaries are the most valuable.

### In scope

**The collection boundary.** Wake collects only from projects that have been
explicitly enabled, and consent starts collection from the moment it is given.
Anything that causes collection from a project that was never consented to, or
that widens the boundary without the owner asking, is in scope. The boundary
lives in the project table under the state directory and has no key in
`config.toml` — a way to widen it by editing a settings file would be a
vulnerability.

**What lands in the event store.** Records are structurally limited to
identifiers, hashes, timestamps, enums, and counters. Prompts, completions,
tool arguments, source code, repository paths, and repository labels are never
persisted in event records. A path that gets any of that content into a stored
record — including through a malformed or hostile transcript — is in scope.

**Repository identity.** Repository identity is a salted, per-machine HMAC; the
readable project map stays local. Anything that recovers a repository path from
a stored or transmitted hash, or that leaks the per-machine salt, is in scope.

**What leaves the machine.** Remote delivery ships in every build and is off
until `wake remote set` and `wake remote on` are both run. In scope: any
delivery when it was never turned on, any network egress from a command other
than `wake update`/`wake update --check` and an enabled flush, and any payload
carrying content beyond the documented span structure (identity hash, readable
label, timing, model, token counts, outcome). Prompt and completion text must
never appear in a payload. `wake remote flush --dry-run` prints exactly what
the next flush would send; a divergence between that preview and what is
actually sent is a vulnerability.

**Credential handling.** The delivery credential is written `0600` and rejected
on read if its mode is looser or if it sits in a directory another local user
can write to. `wake remote set` never echoes the secret key or the joined
credential and never prints the full URL; in scripted use the credential is
read whole from standard input, never passed as an argument, so it does not
reach the process table or shell history. `wake remote status` is the only
command permitted to name the endpoint host, and it prints the bare host, never
the full URL with its userinfo or path. In scope: any disclosure of a
credential through logs, error messages, terminal echo, `doctor` output, a
crash dump, or file permissions; and any acceptance of a credential store that
a local attacker could have substituted.

**Support output.** `wake doctor` output is meant to be safe to paste into a
support request: safe counters only, never transcript content or repository
paths. Anything that puts sensitive content into `doctor` output is in scope.

**The dashboard**, which is bound to loopback only. A binding that reaches
beyond loopback, or a way to read data through it from another host, is in
scope.

**The install path.** `install.sh` and the released binaries — anything that
lets a third party alter what a user installs.

### Out of scope

- **The endpoint you configure.** Once delivery is on, the collector is your
  trust boundary, not Wake's. Vulnerabilities in Langfuse or any other
  OTLP/HTTP receiver belong to that project. What is in scope is Wake sending
  the wrong thing, sending to the wrong place, or sending when it was not
  turned on.
- **Content in the transcripts themselves.** Wake reads what its harness wrote.
  A credential a developer pasted into a prompt is a matter for that harness
  and that developer; Wake's obligation is not to persist or transmit it.
- **Anything requiring an attacker who is already root, or who already has
  read access to the user's home directory.** Such an attacker can read the
  transcripts directly, so Wake's local files are not the weak link. Weak file
  permissions that expose data to *other, unprivileged* local users remain in
  scope.
- Volume of local disk used by the event store, missing rate limits on a
  collector you run, and denial of service against your own machine.
- Findings from an automated scanner with no demonstrated impact. Please
  include a reproduction.

## Design Notes For Reviewers

Two properties are worth knowing before auditing:

- **Nothing OTLP-shaped is persisted.** The delivery payload is computed from
  the local event store at flush time, held for one POST, then discarded. There
  is no spool of outbound payloads to steal.
- **Delivery ids are derived from `event_id`**, so a retried flush deduplicates
  at the receiver rather than double-counting. Delivery is not an audit trail
  and should not be treated as one.

`WAKE_REMOTE_AUTHORIZATION` overrides the stored credential when set, which is
the supported way to run with no secret on disk. Note that an environment
variable is readable by other processes running as the same user — on a shared
CI runner, prefer that runner's secret store over exporting it broadly.
