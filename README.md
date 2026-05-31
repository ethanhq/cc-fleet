# cc-fleet

Spawn any third-party LLM provider with an Anthropic-compatible API (e.g. DeepSeek,
GLM, Kimi, Qwen, MiniMax) as real Claude Code **agent-team teammates** or one-shot
subagents — driven exactly like native teammates. Your main session's own auth
(OAuth subscription or API key) is untouched; vendor workers bill the vendor API key
via `apiKeyHelper` (the key never enters env, argv, or shell history).

`cc-fleet` is a small Go CLI plus one Claude Code skill. The CLI manages per-vendor
profiles, dispatches API keys via `apiKeyHelper`, and spawns teammate sessions in
tmux panes. The skill teaches Claude Code *when* to delegate work to those teammates.

## Install

**One-line (recommended)**
```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh
```
Downloads the prebuilt binary, installs `cc-fleet` + the `ccf` alias, and adds the
skill via the Claude Code plugin. Flags (after `| sh -s --`): `--skill plugin|global|none`,
`--scope user|project|local`, `--prefix DIR`, `--version vX.Y.Z`.

**npm**
```bash
npm install -g cc-fleet      # or run once: npx cc-fleet
```

**go install**
```bash
go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest
ln -sf "$(go env GOPATH)/bin/cc-fleet" "$(go env GOPATH)/bin/ccf"   # optional ccf alias
```

**Prebuilt tarball** — download from [Releases](https://github.com/ethanhq/cc-fleet/releases):
```bash
tar -xzf cc-fleet-*.tar.gz && cd cc-fleet-*/ && ./install.sh
```

**From source**
```bash
git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet && make install
```

## The skill

The binary is just the CLI. To teach Claude Code *when* to delegate, install the skill
via the plugin (the one-line installer does this by default):
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

## License

[Apache-2.0](LICENSE).
