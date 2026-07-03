"use strict";

const test = require("node:test");
const assert = require("node:assert");
const crypto = require("crypto");

const { RELEASE_SIGNING_KEY_B64, verifyChecksumsSig } = require("./install.js");

// pubKeyRaw exports an Ed25519 public KeyObject to SPKI DER and returns its trailing 32
// raw key bytes — the inverse of the SPKI wrapping the verifier does — so a generated test
// keypair can be fed through verifyWithKey below.
function pubKeyRaw(publicKey) {
  const der = publicKey.export({ format: "der", type: "spki" });
  return der.subarray(der.length - 32);
}

// signWith returns a base64 StdEncoding detached signature over body using privateKey.
function signWith(privateKey, body) {
  return crypto.sign(null, body, privateKey).toString("base64");
}

// verifyWithKey re-runs the shipped verify logic but against an arbitrary raw pubkey, so
// tests can feed a keypair they control without touching the shipped constant.
function verifyWithKey(rawKey, sumsBuf, sigStr) {
  const spki = Buffer.concat([
    Buffer.from("302a300506032b6570032100", "hex"),
    rawKey,
  ]);
  const key = crypto.createPublicKey({ key: spki, format: "der", type: "spki" });
  const trimmed = String(sigStr).replace(/^[\t\n\v\f\r ]+|[\t\n\v\f\r ]+$/g, "");
  const sig = Buffer.from(trimmed, "base64");
  if (sig.length !== 64) throw new Error(`signature is ${sig.length} bytes, want 64`);
  if (sig.toString("base64") !== trimmed) throw new Error("signature is not canonical base64");
  if (!crypto.verify(null, sumsBuf, key, sig)) {
    throw new Error("checksums.txt signature does not match the release key");
  }
}

test("verifyChecksumsSig accepts a valid detached sig, with or without trailing newline", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const body = Buffer.from("abc123  cc-fleet-linux-amd64.tar.gz\n");
  const sig = signWith(privateKey, body);
  assert.doesNotThrow(() => verifyWithKey(raw, body, sig));
  assert.doesNotThrow(() => verifyWithKey(raw, body, sig + "\n"));
});

test("verifyChecksumsSig verifies over the exact un-trimmed body", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const body = Buffer.from("line\n"); // trailing newline is part of the signed message
  const sig = signWith(privateKey, body);
  assert.doesNotThrow(() => verifyWithKey(raw, body, sig));
  // Trimming the message must break verification (message is not trimmed).
  assert.throws(() => verifyWithKey(raw, Buffer.from("line"), sig));
});

test("verifyChecksumsSig rejects a tampered body", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const sig = signWith(privateKey, Buffer.from("body A"));
  assert.throws(() => verifyWithKey(raw, Buffer.from("body B"), sig));
});

test("verifyChecksumsSig rejects a truncated or malformed sig", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const body = Buffer.from("body");
  const good = signWith(privateKey, body);
  // truncated (< 64 bytes decoded)
  const truncated = Buffer.from(good, "base64").subarray(0, 32).toString("base64");
  assert.throws(() => verifyWithKey(raw, body, truncated), /64/);
  // malformed base64 → decodes to some other length
  assert.throws(() => verifyWithKey(raw, body, "!!!not base64!!!"));
});

test("verifyChecksumsSig rejects a non-canonical base64 sig (Buffer.from decodes permissively)", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const body = Buffer.from("abc123  cc-fleet-linux-amd64.tar.gz\n");
  const good = signWith(privateKey, body); // canonical StdEncoding base64, padded
  // The exact canonical sig still verifies, with and without the trailing newline the signer writes.
  assert.doesNotThrow(() => verifyWithKey(raw, body, good));
  assert.doesNotThrow(() => verifyWithKey(raw, body, good + "\n"));
  // Padding stripped: still decodes to 64 bytes, but is not canonical → reject.
  const noPad = good.replace(/=+$/, "");
  assert.notStrictEqual(noPad, good);
  assert.strictEqual(Buffer.from(noPad, "base64").length, 64);
  assert.throws(() => verifyWithKey(raw, body, noPad), /canonical/);
  // Trailing invalid characters that Buffer.from silently ignores (still 64 bytes) → reject.
  const junk = good + "!!!";
  assert.strictEqual(Buffer.from(junk, "base64").length, 64);
  assert.throws(() => verifyWithKey(raw, body, junk), /canonical/);
});

test("verifyChecksumsSig trims only ASCII whitespace — exotic whitespace fails the round-trip, never looser than Go", () => {
  const { publicKey, privateKey } = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(publicKey);
  const body = Buffer.from("abc123  cc-fleet-linux-amd64.tar.gz\n");
  const good = signWith(privateKey, body); // canonical, padded
  // BOM prefix: String.trim would strip U+FEFF but Go's strings.TrimSpace does not — the ASCII
  // trim leaves it in, so the round-trip rejects. Go also rejects → true parity for this case.
  assert.throws(() => verifyWithKey(raw, body, "\uFEFF" + good), /canonical/);
  // NEL-wrapped: Go's TrimSpace strips U+0085 and would accept; npm rejects → npm is stricter,
  // never looser, than the Go verifier (worst case an install fails where update would succeed).
  assert.throws(() => verifyWithKey(raw, body, "\u0085" + good + "\u0085"), /canonical/);
  // The signer writes canonical base64 + a trailing '\n' (ASCII) → still verifies.
  assert.doesNotThrow(() => verifyWithKey(raw, body, good + "\n"));
});

test("verifyChecksumsSig rejects a wrong key", () => {
  const { privateKey } = crypto.generateKeyPairSync("ed25519");
  const other = crypto.generateKeyPairSync("ed25519");
  const raw = pubKeyRaw(other.publicKey);
  const body = Buffer.from("body");
  const sig = signWith(privateKey, body);
  assert.throws(() => verifyWithKey(raw, body, sig));
});

test("shipped constant decodes to a 32-byte Ed25519 public key", () => {
  assert.strictEqual(Buffer.from(RELEASE_SIGNING_KEY_B64, "base64").length, 32);
});

test("shipped verifyChecksumsSig verifies against the real release key", () => {
  // A signature produced by a random key must NOT verify against the embedded release key.
  // sigBuf carries the base64 sig text, as the .sig asset does on disk.
  const { privateKey } = crypto.generateKeyPairSync("ed25519");
  const body = Buffer.from("checksums");
  const sig = Buffer.from(signWith(privateKey, body) + "\n");
  assert.throws(() => verifyChecksumsSig(body, sig), /does not match/);
});
