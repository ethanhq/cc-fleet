#!/usr/bin/env node
// Thin launcher: exec the platform binary that postinstall placed next to this
// file, passing through argv and the exit code. Both `cc-fleet` and `ccf` point
// here, so `ccf` is just the same binary under a shorter name.
"use strict";

const path = require("path");
const fs = require("fs");
const { spawnSync } = require("child_process");

const binName = process.platform === "win32" ? "cc-fleet.exe" : "cc-fleet";
const bin = path.join(__dirname, binName);

if (!fs.existsSync(bin)) {
  console.error(
    "cc-fleet: the platform binary is missing — the postinstall download may " +
      "have failed. Reinstall with `npm install -g @ethanhq/cc-fleet`, or grab a release " +
      "from https://github.com/ethanhq/cc-fleet/releases."
  );
  process.exit(1);
}

const res = spawnSync(bin, process.argv.slice(2), { stdio: "inherit" });
if (res.error) {
  console.error(`cc-fleet: failed to run ${bin}: ${res.error.message}`);
  process.exit(1);
}
process.exit(res.status === null ? 1 : res.status);
