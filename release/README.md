# cc-fleet (prebuilt release)

A prebuilt `cc-fleet` binary plus the `cc-fleet` skill. No Go toolchain
needed — this archive ships a compiled binary for your platform.

## Install

```bash
# 1. install the binary (+ ccf alias) and the skill (skill via the plugin by default)
./install.sh --prefix ~/.local/bin

# 2. add a provider — pipe the key on stdin, never inline it in argv (it leaks
#    to shell history / `ps`)
printf '%s' "$DEEPSEEK_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

The config tree is created automatically on first use — `cc-fleet init` is optional
(run it only to create the tree and health-check up front). Then `cc-fleet doctor` to
health-check, `cc-fleet list` to see what's configured, and `cc-fleet update` to update
the binary + plugin later.

> **Skill channel.** By default `install.sh` installs the skills via the Claude Code
> plugin (`--skill plugin`). Use `--skill global` to copy the bundled per-lane skills
> into `~/.claude/skills/cc-fleet-{subagent,team,workflow}/` instead (handy offline), or
> `--skill none` for the binary only. Pick **one** channel so you don't end up with two copies.

## What's in this archive

| File | Purpose |
|---|---|
| `cc-fleet` | The prebuilt binary for this platform. |
| `install.sh` | Copy-binary installer (no build). Skills via the plugin by default; `--skill global` copies the bundled skills, `--skill none` skips them. For a from-source build, clone the repo and run `make install`. |
| `skills/` | The bundled per-lane skills (`subagent` / `team` / `workflow`) + `shared/` docs — used by `--skill global` (the default install uses the plugin instead). |

Full documentation: https://github.com/ethanhq/cc-fleet
