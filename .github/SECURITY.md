# Security Policy

cc-fleet's core promise is that provider API keys and OAuth tokens never leak — not into
env, argv, shell history, logs, or the UI. Reports that break that promise are treated as
critical.

## Supported versions

The latest release only. `cc-fleet update` (or the npm/zip channel on Windows) moves you to
it.

## Reporting a vulnerability

Please **do not open a public issue** for anything security-sensitive. Instead, use GitHub's
private vulnerability reporting:
[Report a vulnerability](https://github.com/ethanhq/cc-fleet/security/advisories/new).
You can expect an acknowledgement within a week.

In scope, with examples:

- a provider key or OAuth bearer reaching env, argv, `ps` output, a log line, an error
  message, or any UI surface unmasked;
- the conversion daemon exposing the upstream bearer outside the loopback process;
- path traversal or shell injection through provider/team/agent names (they flow into file
  paths and the `apiKeyHelper` string);
- a spawned worker gaining access to the main session's own credentials.

Provider-side issues (a vendor's API leaking data, model behavior) are out of scope — report
those to the provider.
