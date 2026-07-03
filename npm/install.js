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

// Base64 Ed25519 PUBLIC key the release pipeline signs checksums.txt with. It mirrors
// releaseSigningKeyB64 in internal/selfupdate/signing.go; signing-preflight.sh fails the
// release unless the two literals (and the key derived from the signing secret) match.
const RELEASE_SIGNING_KEY_B64 = "dAbMy8Omb0En+n0xZNGTjKsNHDdwBipwqy+jHXGpZjw=";

// verifyChecksumsSig verifies a base64 Ed25519 detached signature (sigBuf) over the EXACT
// bytes of checksums.txt (sumsBuf) against the embedded release public key. It is the trust
// anchor checked before any sha256 is trusted, and fails closed on a malformed embedded key,
// a malformed/wrong-length signature, or a verification mismatch — a mirror / redirect /
// arbitrary CCF_BASE_URL cannot forge it.
function verifyChecksumsSig(sumsBuf, sigBuf) {
  const raw = Buffer.from(RELEASE_SIGNING_KEY_B64, "base64");
  if (raw.length !== 32) {
    throw new Error("release signing key unavailable (malformed embedded public key)");
  }
  // Wrap the raw 32-byte key in a DER SPKI header so createPublicKey accepts it.
  const spki = Buffer.concat([
    Buffer.from("302a300506032b6570032100", "hex"),
    raw,
  ]);
  const key = crypto.createPublicKey({ key: spki, format: "der", type: "spki" });
  // Trim only ASCII whitespace (String.trim strips U+FEFF/BOM, Go's strings.TrimSpace strips
  // U+0085/NEL — they disagree at the edges); any exotic whitespace then survives into the
  // canonical round-trip below and fails it, so this path is never more permissive than the
  // Go verifier.
  const trimmed = sigBuf.toString("utf8").replace(/^[\t\n\v\f\r ]+|[\t\n\v\f\r ]+$/g, "");
  const sig = Buffer.from(trimmed, "base64");
  if (sig.length !== 64) {
    throw new Error(`signature is ${sig.length} bytes, want 64`);
  }
  // Buffer.from decodes base64 permissively (ignoring stray characters and missing padding);
  // the Go verifier's base64.StdEncoding.DecodeString rejects both. Require a canonical
  // round-trip so a corrupted .sig cannot pass here yet be rejected by `cc-fleet update`.
  if (sig.toString("base64") !== trimmed) {
    throw new Error("signature is not canonical base64");
  }
  if (!crypto.verify(null, sumsBuf, key, sig)) {
    throw new Error("checksums.txt signature does not match the release key");
  }
}

const PLATFORM = { linux: "linux", darwin: "darwin", win32: "windows" }[
  process.platform
];
const ARCH = { x64: "amd64", arm64: "arm64" }[process.arch];
const WIN = process.platform === "win32";

if (!PLATFORM || !ARCH) {
  console.error(
    `cc-fleet: unsupported platform ${process.platform}/${process.arch} ` +
      "(cc-fleet supports linux|darwin|win32 on x64|arm64)"
  );
  process.exit(1);
}

// Windows ships a .zip; linux/darwin a .tar.gz. The binary inside gains .exe
// on Windows (goreleaser appends it).
const archive = WIN
  ? `cc-fleet-${PLATFORM}-${ARCH}.zip`
  : `cc-fleet-${PLATFORM}-${ARCH}.tar.gz`;
const binName = WIN ? "cc-fleet.exe" : "cc-fleet";
// CCF_BASE_URL overrides the asset base for an https mirror or a local https test; a
// non-https override is rejected so the source cannot be silently re-pointed at plaintext.
const base = (() => {
  const override = process.env.CCF_BASE_URL;
  if (override) {
    let u;
    try {
      u = new URL(override);
    } catch {
      throw new Error(`CCF_BASE_URL must be an https:// URL, got ${JSON.stringify(override)}`);
    }
    if (u.protocol !== "https:") {
      throw new Error(`CCF_BASE_URL must be an https:// URL, got ${JSON.stringify(override)}`);
    }
    return override;
  }
  return `https://github.com/${REPO}/releases/download/v${version}`;
})();

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

  const [archiveBuf, sumsBuf, sigBuf] = await Promise.all([
    get(`${base}/${archive}`),
    get(`${base}/checksums.txt`),
    get(`${base}/checksums.txt.sig`),
  ]);

  // Verify the release signature over checksums.txt against the embedded public key BEFORE
  // trusting any sha256: the checksum is same-channel and only proves the archive matches a
  // hash fetched from the same place; the signature is the trust anchor. Fail closed.
  verifyChecksumsSig(sumsBuf, sigBuf);

  const expected = checksumFor(sumsBuf.toString("utf8"), archive);
  if (!expected) throw new Error(`no checksum for ${archive} in checksums.txt`);
  const actual = crypto.createHash("sha256").update(archiveBuf).digest("hex");
  if (actual !== expected) {
    throw new Error(`checksum mismatch for ${archive}`);
  }

  const tmp = fs.mkdtempSync(path.join(os.tmpdir(), "cc-fleet-"));
  try {
    const archivePath = path.join(tmp, archive);
    fs.writeFileSync(archivePath, archiveBuf);
    // tar reads both .tar.gz and .zip; bsdtar (Windows >=1809, and the GH
    // runners) handles the zip with -xf.
    execFileSync("tar", [WIN ? "-xf" : "-xzf", archivePath, "-C", tmp]);
    const extracted = path.join(tmp, `cc-fleet-${PLATFORM}-${ARCH}`, binName);
    const dest = path.join(binDir, binName);
    fs.copyFileSync(extracted, dest);
    if (!WIN) fs.chmodSync(dest, 0o755);
    // Install manifest (co-located with the binary): `cc-fleet update` reads it
    // and delegates to npm instead of self-replacing an npm-managed binary.
    fs.writeFileSync(
      path.join(binDir, ".cc-fleet-install.json"),
      JSON.stringify({ method: "npm" })
    );
  } finally {
    fs.rmSync(tmp, { recursive: true, force: true });
  }
  console.log(`cc-fleet: installed v${version} (${PLATFORM}/${ARCH})`);
  console.log(
    "cc-fleet: this installs the BINARY only. To let Claude Code use it, also install the skill:\n" +
      "            claude plugin marketplace add ethanhq/cc-fleet\n" +
      "            claude plugin install cc-fleet@ethanhq\n" +
      "          Then run `cc-fleet` to register a provider."
  );
}

if (require.main === module) {
  main().catch((err) => {
    console.error(`cc-fleet: install failed: ${err.message}`);
    process.exit(1);
  });
}

module.exports = { RELEASE_SIGNING_KEY_B64, verifyChecksumsSig, checksumFor };
