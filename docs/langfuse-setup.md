# Langfuse setup

Wake sends OTLP spans straight to Langfuse. This takes about five minutes.

## 1. Create a dedicated project

In Langfuse, create a new project used only for wake (for example `wake-telemetry`). Do not reuse a
project that holds anything else: the keys you create below can read everything in it.

## 2. Make the keys and the header

In the project's settings, create an API key pair: a public key (`pk-lf-...`) and a secret key
(`sk-lf-...`). Langfuse authenticates OTLP with HTTP Basic auth, so the header value is
`Basic ` followed by `base64(public_key:secret_key)`:

```sh
printf 'pk-lf-...:sk-lf-...' | base64
```

The header is `Authorization: Basic <that output>`.

## 3. Find the endpoint

```
https://<langfuse host>/api/public/otel/v1/traces
```

Use your Langfuse Cloud region host or your self-hosted host. Wake sends OTLP over HTTP with JSON.

## 4. Give it to wake

**One machine:**

```sh
wake install \
  --endpoint "https://<langfuse host>/api/public/otel/v1/traces" \
  --header "Authorization=Basic <base64>"
```

**An organisation (APM):** put the same values in the package's `bin/telemetry.json`, copied from
`telemetry.example.json`. See [package/README.md](../package/README.md).

## 5. Check

```sh
wake check
```

It prints the endpoint host and the number of headers set, then sends a test span. Open the project's
Traces page: a `wake.command check` span should appear within a few seconds.

## The three conditions

Langfuse's secret key also gives read access to that project through its API, so the key pair that wake
ships to every developer is not write-only. Distributing it is acceptable only if:

1. **The Langfuse project is dedicated to wake**, so the keys reach nothing else.
2. **The package repository is private.** Anyone who has the package can read the project's telemetry.
3. **Rotation means new keys and a new package version.** Issue new keys, update `telemetry.json`, and
   release the package again.

If those stop being acceptable, put an OTel Collector in front of Langfuse with a write-only token. The
change is configuration only.
