# cc-fleet

Spawn any third-party LLM provider with an Anthropic-compatible API (e.g. DeepSeek,
GLM, Kimi, Qwen, MiniMax) as real Claude Code **agent-team teammates** or one-shot
subagents — driven just like native teammates. Your main session's own auth (OAuth
subscription or API key) is untouched; vendor workers bill the vendor key.

## Install

```bash
npm install -g cc-fleet
# or run without installing:
npx cc-fleet --help
```

`postinstall` downloads the prebuilt binary for your platform (linux/darwin ×
x64/arm64) from the matching GitHub Release, verifies its sha256, and installs
`cc-fleet` plus the `ccf` alias.

## The skill

The npm package installs only the CLI binary. To teach Claude Code *when* to use
the fleet, install the cc-fleet skill via the plugin:

```bash
claude plugin marketplace add ethanhq/cc-fleet
claude plugin install cc-fleet@ethanhq
```

## First run

```bash
cc-fleet init        # create config at ~/.config/cc-fleet/
cc-fleet add <vendor> ...    # register a vendor
cc-fleet doctor      # health-check
```

Full documentation: https://github.com/ethanhq/cc-fleet
