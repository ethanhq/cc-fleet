# Troubleshooting — spawn failures + fingerprint self-heal

Read this on a `cc-fleet spawn --json` failure (`ok:false`), or when a spawn returns `FINGERPRINT_MISSING` / `SPAWN_DID_NOT_SETTLE`. (Subagent failure codes live in `references/subagent.md`; hide/show codes in `references/teammates.md`.)

## Contents
- Spawn `error_code` dispatch table
- General rules
- Self-heal flow (the 6 steps) + worked example

---

## Failure handling

`cc-fleet spawn --json` always emits exactly one JSON envelope on stdout. On failure:
```json
{"ok":false,"error_code":"<CODE>","error_msg":"<human msg>","vendor":"<v>","suggestion":"<hint>"}
```
Dispatch on `error_code` — do **not** parse `error_msg` prose.

| `error_code` | What it means | What you do |
|---|---|---|
| `VENDOR_UNREACHABLE` | DNS / connect / timeout to vendor models endpoint (3s probe failed). | Suggest `cc-fleet doctor`; if urgent, fall back to native `Agent({model: 'sonnet'})` and tell the user the vendor is sick. |
| `KEY_INVALID` | Vendor returned HTTP 401 — key wrong/expired. | Tell the user: re-add via `cc-fleet edit <vendor> --api-key-stdin <<<"$NEW_KEY"` (file backend) or rotate in the secret manager and re-run. Don't retry without user action. **Never** put the raw key on the command line. |
| `MODEL_NOT_FOUND` | The requested `--model` isn't cached and the vendor rejected it. | `cc-fleet refresh <vendor> --json`, then retry. If still failing, drop `--model` to use the vendor's `default_model`. |
| `FINGERPRINT_MISSING` | **Rare.** A fresh install ships a bundled recipe, so the first spawn just works. This means an EXISTING `~/.config/cc-fleet/fingerprint.json` is corrupt/unreadable. | Walk the **self-heal flow** below to re-capture, or tell the user to remove the corrupt cache. |
| `FINGERPRINT_STALE` | **Rare.** No longer fires on a CC upgrade (binary path resolved live). This means NO `claude` binary could be found at all. | Tell the user to install Claude Code / fix PATH; `cc-fleet doctor` confirms. The self-heal flow can't help if there's no binary to probe. |
| `SPAWN_DID_NOT_SETTLE` | The pane was created but the teammate process exited during startup — almost always a spawn-recipe mismatch on a Claude Code NEWER than cc-fleet's bundled recipe (a drifted/renamed flag). The spawn was already rolled back. | Walk the **self-heal flow** — the probe captures the current CC's recipe, which overrides the bundled default. Then retry. |
| `NO_LEAD_SESSION` | The team has no lead session id — you forgot `TeamCreate`, or the config is corrupted. | Run `TeamCreate({team_name})` (native), then retry. If the team exists but is broken, `cc-fleet teardown <team>` and re-create. |
| `TEAM_NOT_FOUND` | The `--team` doesn't exist and `--auto-team` was off. | Run `TeamCreate({team_name})` first, or retry with `--auto-team` (default on). |
| `PANE_CREATION_FAILED` | tmux `split-window` returned non-zero (in-tmux path). | Check tmux is running. If NOT inside tmux (`$TMUX` empty), spawn auto-builds an out-of-tmux swarm session instead — this error only fires on genuine in-tmux split failures. (If the team already has a live swarm under the same name, tear it down or use a fresh team name.) |
| `UNKNOWN_VENDOR` | The vendor name isn't in `vendors.toml`. | `cc-fleet list --json` to see configured vendors; tell the user to `cc-fleet add <vendor>` first. Don't guess. |
| `VENDOR_DISABLED` | The vendor row has `enabled = false`. | Pick a different vendor or tell the user to `cc-fleet edit <vendor> --enable`. |
| (rate-limit class) | Vendor returns 429 / rate-limit text in `error_msg`. | Wait 30–60s and retry once; if it persists, switch vendor. Don't loop tightly. |

## General rules
- One retry max for transient failures (`VENDOR_UNREACHABLE` after `doctor`, rate-limit class).
- For config-level failures (`UNKNOWN_VENDOR`, `KEY_INVALID`, `VENDOR_DISABLED`), surface to the user and stop — don't re-run blindly.
- For `FINGERPRINT_MISSING` (corrupt existing cache) and `SPAWN_DID_NOT_SETTLE` (recipe drift on a newer CC), run the self-heal flow before retrying. For `FINGERPRINT_STALE` (no `claude` binary anywhere) the self-heal flow can't help — install/fix Claude Code or PATH, then retry.
- **Spawn errors ≠ runtime errors.** This table is for `cc-fleet spawn` *failing up front*. A spawn that *succeeds* and then the teammate wedges on a `429` / balance / `401` mid-task produces **no error envelope and no idle notification** — that case is "Watching for stuck vendor teammates" in `references/teammates.md` (poll `cc-fleet ps --json --check`; never wait open-endedly).

---

## Self-heal flow (FINGERPRINT_MISSING corrupt-cache · SPAWN_DID_NOT_SETTLE recipe-drift)

cc-fleet can't invoke the native `Agent` tool itself — only you can. So when the spawn recipe needs (re)capturing, **you orchestrate a probe** while cc-fleet captures the recipe from the probe (Linux: `/proc`; macOS: `ps`).

**This is rare.** cc-fleet ships a bundled default recipe and resolves the binary path live, so a fresh install and a CC version upgrade both "just work" with no probe. You only run this flow on `FINGERPRINT_MISSING` (existing cache corrupt) or `SPAWN_DID_NOT_SETTLE` (a CC newer than the bundled recipe rejected a drifted flag). The flow takes ~15–20s. (macOS: cc-fleet reads the probe's argv via `ps`, not `/proc`; the flow is otherwise identical, and a fresh install never needs it — the bundled recipe covers it.)

### The six steps
```
1. DETECT — spawn returned ok:false with error_code in
   {FINGERPRINT_MISSING, SPAWN_DID_NOT_SETTLE}. (FINGERPRINT_STALE is NOT in this
   set — it means no claude binary; the probe can't help, fix Claude Code / PATH.)
   Do NOT retry the original spawn yet.

2. CREATE PROBE TEAM — generate a short uuid suffix; pick a non-colliding name.
   TeamCreate({team_name: "_ccf-probe-<uuid>"})                    ← native

3. SPAWN PROBE TEAMMATE (native, NOT a vendor) — a vanilla native teammate whose
   only job is to exist long enough for cc-fleet to read its recipe (`/proc` on Linux, `ps` on macOS).
   Agent({subagent_type: "general-purpose", name: "probe",
          team_name: "_ccf-probe-<uuid>", prompt: "Print READY and idle.",
          run_in_background: true})

4. WAIT FOR PROBE TO BE LIVE — give it ~10–15s to fully start (watch for its idle
   notification, or sleep ~12s). Do NOT proceed before the process is running, or
   pgrep won't find it.

5. SNAPSHOT FINGERPRINT
   cc-fleet refresh-fingerprint --probe-team _ccf-probe-<uuid> --json
   Expect: {"ok":true,"fingerprint_path":"<...>","cc_version":"<...>","captured_at":"..."}
   On failure: PROBE_NOT_FOUND → probe not started yet, wait 5s and retry once ·
   CAPTURE_FAILED → probe argv/env read failed (Linux `/proc`; macOS `ps`), report + abort · SAVE_FAILED → disk/perm,
   report + abort.

6. CLEAN UP PROBE TEAM — TeamDelete() (your main session holds the lead if you ran
   TeamCreate here). If not appropriate, Bash: cc-fleet teardown _ccf-probe-<uuid> --json

7. RETRY ORIGINAL SPAWN — re-run the original cc-fleet spawn; it should now succeed.
```

### Worked example
```bash
# 1. detected: {"ok":false,"error_code":"FINGERPRINT_MISSING",...}
TeamCreate({team_name: "_ccf-probe-7a3f1e2c"})                                 # 2 native
Agent({subagent_type: "general-purpose", name: "probe",                       # 3 native
       team_name: "_ccf-probe-7a3f1e2c", prompt: "Print READY and idle.",
       run_in_background: true})
# 4. wait ~12s (or until the probe's idle notification)
cc-fleet refresh-fingerprint --probe-team _ccf-probe-7a3f1e2c --json           # 5 Bash
# → {"ok":true,"fingerprint_path":"…/fingerprint.json","cc_version":"2.1.150",...}
TeamDelete()   # 6 native  (or: cc-fleet teardown _ccf-probe-7a3f1e2c --json)
cc-fleet spawn deepseek --as worker-1 --team refactor-api --json              # 7 retry → ok:true
```

**Do not skip step 4.** `pgrep` won't see the probe until its claude process is actually running. If step 5 returns `PROBE_NOT_FOUND`, wait another 5s and retry the `refresh-fingerprint` call — do not re-spawn the probe.
