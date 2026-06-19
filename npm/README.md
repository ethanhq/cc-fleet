# cc-fleet

Run any model with an Anthropic- or OpenAI-compatible API — DeepSeek, GLM, Kimi,
Qwen, MiniMax, even your Codex subscription — as scripted multi-agent **workflows**,
real Claude Code **agent-team teammates**, or one-shot **subagents**, driven just
like native ones. Your main session's own auth (OAuth subscription or API key) is
untouched; each worker bills its own provider — an API key, or your Codex subscription.

No Claude subscription? `ccf run <provider>` starts an interactive session on that
provider — the same `claude` you know, just on the provider's model.

## Install

```bash
npm install -g @ethanhq/cc-fleet
# or run without installing:
npx @ethanhq/cc-fleet --help
```

`postinstall` downloads the prebuilt release archive for your platform (linux/darwin/windows
× x64/arm64) from the matching GitHub Release, verifies its sha256 against `checksums.txt`,
then unpacks the binary — giving you `cc-fleet` and the `ccf` alias.

## The skill

The npm package installs only the CLI binary. To teach Claude Code *when* to reach for
the fleet — scheduling Workflows, Agent Teams, and Subagents on the right lane — install
the cc-fleet plugin. Either way:

- **Inside Claude Code (recommended)** — launch `claude` and run the two slash commands
  (or `/plugin` for the interactive panel):

  ```
  /plugin marketplace add ethanhq/cc-fleet
  /plugin install cc-fleet@ethanhq
  ```

- **From the shell:**

  ```bash
  claude plugin marketplace add ethanhq/cc-fleet
  claude plugin install cc-fleet@ethanhq
  ```

## First run

```bash
cc-fleet             # open the TUI and register a provider (config created on first save)
```

## Common commands

`cc-fleet` and `ccf` are the same binary — use whichever you prefer.

```bash
ccf doctor               # health-check (core vs optional)
ccf list                 # list configured providers
ccf run <provider>       # interactive claude session on that provider
ccf update               # update the binary + plugin (--check to only check)
ccf uninstall            # reset cc-fleet state (--all to fully remove)
```

Full documentation: https://github.com/ethanhq/cc-fleet
