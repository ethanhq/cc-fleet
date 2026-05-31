# cc-fleet (prebuilt release)

A prebuilt `cc-fleet` binary plus the `cc-fleet` skill. No Go toolchain
needed — this archive ships a compiled binary for your platform.

## Install (3 steps)

```bash
# 1. install the binary (+ ccf alias) and the skill
./install.sh --prefix ~/.local/bin

# 2. first-time setup
cc-fleet init

# 3. add a vendor — pipe the key on stdin, never inline it in argv (it leaks
#    to shell history / `ps`)
printf '%s' "$DEEPSEEK_KEY" | cc-fleet add deepseek \
  --base-url https://api.deepseek.com/anthropic \
  --models-endpoint https://api.deepseek.com/v1/models \
  --default-model deepseek-v4-flash \
  --secret-backend file --secret-ref deepseek.key --api-key-stdin
```

Then `cc-fleet doctor` to health-check and `cc-fleet list` to see what's configured.

> **Already using the cc-fleet plugin?** The plugin delivers the skill, so install
> the binary only — `./install.sh --no-skill` — to avoid two copies. Pick **one**
> skill channel (the plugin **or** this archive's `install.sh`), not both.

## What's in this archive

| File | Purpose |
|---|---|
| `cc-fleet` | The prebuilt binary for this platform. |
| `install.sh` | Copy-binary installer (no build). Installs the skill by default; `--no-skill` skips it (for plugin users). For a from-source build, use the repo's top-level `install.sh`. |
| `SKILL.md` | The `cc-fleet` skill, installed to `~/.claude/skills/cc-fleet/`. |
| `references/` | Skill reference docs (progressive disclosure), installed alongside `SKILL.md`. |

Full documentation: https://github.com/ethanhq/cc-fleet
