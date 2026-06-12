#!/usr/bin/env node
// cc-fleet plugin · SessionStart binary check.
//
// This plugin ships ONLY the skill (+ this hook). The
// native `cc-fleet` Go binary is installed SEPARATELY onto PATH — a plugin
// can't compile or reliably ship a native binary (no install-time hook; the
// plugin root path is ephemeral and would break the apiKeyHelper pin baked
// into provider profiles).
//
// If the binary is missing, the skill can still route a request but every CLI
// call it makes will fail, so we nudge once at session start.
//
// Contract: SessionStart hooks must NEVER block the session — we always exit 0.
//   - binary present  -> print nothing (no context noise)
//   - binary missing  -> one-line install hint on stdout (Claude Code folds it
//                        into the session context and shows the user)
//
// Node ships with Claude Code, so this runs on every platform; on win32 the
// PATH probe honors PATHEXT (cc-fleet.exe / .cmd / …).
"use strict";

const fs = require("fs");
const path = require("path");

function onPath() {
  const dirs = (process.env.PATH || "").split(path.delimiter).filter(Boolean);
  const win = process.platform === "win32";
  // On Windows a bare name resolves through PATHEXT; elsewhere it's the file
  // itself (an executable bit is not required for "is it findable").
  const exts = win
    ? (process.env.PATHEXT || ".COM;.EXE;.BAT;.CMD").split(";").filter(Boolean)
    : [""];
  for (const dir of dirs) {
    for (const ext of exts) {
      const candidate = path.join(dir, "cc-fleet" + ext);
      try {
        if (fs.statSync(candidate).isFile()) return true;
      } catch {
        // not here — keep looking
      }
    }
  }
  return false;
}

if (!onPath()) {
  console.log(
    "cc-fleet binary not found on PATH — the cc-fleet skills need it to spawn provider teammates/subagents. Install: go install github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest  (or grab a release / see https://github.com/ethanhq/cc-fleet#install)."
  );
}
process.exit(0);
