# Install & maintenance

Every cc-fleet install channel plus day-to-day maintenance. The fastest path is in the [README quickstart](../README.md#install); this page covers the rest, the installer flags, and the maintenance commands.

## Prerequisite: Claude Code

cc-fleet drives the official `claude` CLI — install it if it isn't on your PATH (skip if it is):

- **macOS / Linux**: `curl -fsSL https://claude.ai/install.sh | bash`
- **Windows (PowerShell)**: `irm https://claude.ai/install.ps1 | iex`

## One-line script overrides

The [README](../README.md#install) shows the bare one-liner. To override the installer's defaults, append flags after `| sh -s --`:

```bash
curl -fsSL https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.sh | sh -s -- \
  --skill plugin \        # how to add the skill: plugin (default, via marketplace) | global (copy into ~/.claude/skills) | none
  --scope user \          # plugin scope: user (default) | project | local
  --prefix ~/.local/bin \ # where to install the binary (default: ~/.local/bin)
  --version v0.2.1         # pin a specific release (default: latest)
```

On Windows the PowerShell installer takes the same overrides via environment variables:

```powershell
$env:CCF_VERSION = "v0.2.1"; $env:CCF_PREFIX = "$HOME\bin"; irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
```

Re-running `install.sh` (or a release archive's `./install.sh`) over an existing install is safe even while cc-fleet is running — a teammate's `apiKeyHelper`, a background `ccf watch`: the new binary is copied to a temp file next to the old one and renamed over it, so the running process keeps its file and the new one starts cleanly. (Copying over the file in place makes macOS kill every later launch of it.)

## From another source

These channels install the **CLI only** — you then add the plugin by hand. npm / go / Releases work on Linux, macOS, and Windows alike:

| Channel | Command |
|------|------|
| npm | `npm install -g @ethanhq/cc-fleet` |
| go install | `go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest` |
| [Releases](https://github.com/ethanhq/cc-fleet/releases) | download the archive for your OS / arch, then `tar -xzf cc-fleet-*.tar.gz && cd cc-fleet-*/ && ./install.sh` (Windows: unzip and run the bundled `install.ps1`) |
| Source *(Linux / macOS)* | `git clone https://github.com/ethanhq/cc-fleet.git && cd cc-fleet && make install` — needs `make`; from source on Windows, use the `go install` row above |

### Add the Claude Code plugin

This is what teaches Claude *when* to delegate: its three skills teach it to schedule Workflows, Agent Teams, and Subagents in the right situations and pick the right lane. Either way:

- **Inside Claude Code (recommended):** launch `claude` and type the two slash commands (or run `/plugin` for the interactive Marketplaces / Discover panel):
  ```
  /plugin marketplace add ethanhq/cc-fleet
  /plugin install cc-fleet@ethanhq
  ```
- **From the CLI:**
  ```bash
  claude plugin marketplace add ethanhq/cc-fleet
  claude plugin install cc-fleet@ethanhq        # installs at user scope; pass --scope project|local to change
  ```

## Requirements

Agent Team mode **can't run on Windows**; everything else (Subagent / Workflow / run / TUI) behaves identically with no extra dependency. Provider teammates need:

- **Claude Code ≥ 2.1.278**, the terminal `claude` — not the Claude desktop app, `claude -p`, or an SDK session, which have no agent team;
- **tmux or iTerm2** around that `claude`, so teammates open in panes. Install tmux first (macOS: `brew install tmux`; Debian / Ubuntu: `sudo apt install tmux`) and start `claude` inside it;
- **a one-time setup**, then a `claude` restart (the TUI offers to do it on first run):

  ```bash
  ccf teammate setup           # shows what it would change
  ccf teammate setup --yes --teammate-mode tmux
  ```

  It writes the launcher `~/.config/cc-fleet/bin/claude-teammate`, the `ccf-*` agent definitions in `~/.claude/agents/`, and two keys (plus `teammateMode`, when asked) in `~/.claude/settings.json`. `ccf teammate setup --remove --yes` undoes it; `ccf repair` re-pins the launcher after cc-fleet moves; `ccf uninstall` removes it too.

## Maintenance

```bash
ccf doctor               # health check: dependencies, providers, plugin status, teammate lane
ccf repair               # rewrite provider profiles; re-pin the teammate launcher
ccf update               # self-update by install channel + refresh the plugin
ccf update rollback      # roll back to the previous version
ccf uninstall            # reset config & state (keeps the binary and secrets)
ccf uninstall --all      # also remove the binary and plugin
```

`ccf update` reads the manifest each installer leaves behind: a script / manual install swaps the binary in place (sha256-verified, `--version` smoke-tested), npm / go delegate to their package manager, and the plugin is refreshed in the same pass. Only a comparable release version ever updates; a dev build is left alone.
