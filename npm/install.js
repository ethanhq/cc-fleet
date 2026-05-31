#!/usr/bin/env node
// postinstall: download the prebuilt cc-fleet binary for this platform from the
// matching GitHub Release, verify its sha256, and unpack it into bin/.
"use strict";

const https = require("https");
const fs = require("fs");
const os = require("os");
const path = require("path");
const crypto = require("crypto");
const { execFileSync } = require("child_process");

const REPO = "ethanhq/cc-fleet";
const version = require("./package.json").version;

const PLATFORM = { linux: "linux", darwin: "darwin" }[process.platform];
const ARCH = { x64: "amd64", arm64: "arm64" }[process.arch];

if (!PLATFORM || !ARCH) {
  console.error(
    `cc-fleet: unsupported platform ${process.platform}/${process.arch} ` +
      "(cc-fleet supports linux|darwin on x64|arm64)"
  );
  process.exit(1);
}

const tarball = `cc-fleet-${PLATFORM}-${ARCH}.tar.gz`;
// CCF_BASE_URL overrides the asset base for a mirror or a local test.
const base =
  process.env.CCF_BASE_URL ||
  `https://github.com/${REPO}/releases/download/v${version}`;

// GET that follows redirects (GitHub release assets redirect to a CDN).
function get(url, redirects = 0) {
  return new Promise((resolve, reject) => {
    if (redirects > 10) return reject(new Error("too many redirects"));
    https
      .get(url, { headers: { "User-Agent": "cc-fleet-npm" } }, (res) => {
        if (res.statusCode >= 300 && res.statusCode < 400 && res.headers.location) {
          res.resume();
          return resolve(get(res.headers.location, redirects + 1));
        }
        if (res.statusCode !== 200) {
          res.resume();
          return reject(new Error(`GET ${url} -> HTTP ${res.statusCode}`));
        }
        const chunks = [];
        res.on("data", (c) => chunks.push(c));
        res.on("end", () => resolve(Buffer.concat(chunks)));
      })
      .on("error", reject);
  });
}

function checksumFor(sumsText, name) {
  for (const line of sumsText.split("\n")) {
    const parts = line.trim().split(/\s+/);
    if (parts[1] === name) return parts[0];
  }
  return null;
}

async function main() {
  const binDir = path.join(__dirname, "bin");
  fs.mkdirSync(binDir, { recursive: true });

  const [tarBuf, sumsBuf] = await Promise.all([
    get(`${base}/${tarball}`),
    get(`${base}/checksums.txt`),
  ]);

  const expected = checksumFor(sumsBuf.toString("utf8"), tarball);
  if (!expected) throw new Error(`no checksum for ${tarball} in checksums.txt`);
  const actual = crypto.createHash("sha256").update(tarBuf).digest("hex");
  if (actual !== expected) {
    throw new Error(`checksum mismatch for ${tarball}`);
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "cc-fleet-"));
  try {
    const tarPath = path.join(tmp, tarball);
    fs.writeFileSync(tarPath, tarBuf);
    execFileSync("tar", ["-xzf", tarPath, "-C", tmp]);
    const extracted = path.join(tmp, `cc-fleet-${PLATFORM}-${ARCH}`, "cc-fleet");
    const dest = path.join(binDir, "cc-fleet");
    fs.copyFileSync(extracted, dest);
    fs.chmodSync(dest, 0o755);
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  console.log(`cc-fleet: installed v${version} (${PLATFORM}/${ARCH})`);
}

main().catch((err) => {
  console.error(`cc-fleet: install failed: ${err.message}`);
  process.exit(1);
});
