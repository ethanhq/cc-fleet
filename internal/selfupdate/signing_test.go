package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func b64(b []byte) []byte { return []byte(base64.StdEncoding.EncodeToString(b)) }

// TestEmbeddedReleaseKeyValid guards that the shipped releaseSigningKeyB64 constant is a
// well-formed Ed25519 public key — a placeholder/zero/malformed value would make every
// signed update fail closed, so it must never ship.
func TestEmbeddedReleaseKeyValid(t *testing.T) {
	if len(releaseVerifyKey) != ed25519.PublicKeySize {
		t.Fatalf("embedded release public key is malformed (len=%d) — set a valid base64 Ed25519 key in signing.go", len(releaseVerifyKey))
	}
}

func TestVerifyChecksumsSig(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	old := releaseVerifyKey
	releaseVerifyKey = pub
	t.Cleanup(func() { releaseVerifyKey = old })

	sums := []byte("c0ffee  cc-fleet-linux-amd64.tar.gz\n")
	sig := append(b64(ed25519.Sign(priv, sums)), '\n') // trailing newline as the signer writes it

	if err := verifyChecksumsSig(sums, sig); err != nil {
		t.Fatalf("AC1: a valid signature should verify: %v", err)
	}

	cases := []struct {
		name      string
		sums, sig []byte
	}{
		{"AC3 tampered checksums", []byte("tampered\n"), sig},
		{"AC4 signature from a non-release key", sums, signWithFreshKey(t, sums)},
		{"malformed base64 signature", sums, []byte("!!!not-base64")},
		{"wrong-length signature", sums, b64([]byte("too short"))},
		{"AC2 empty signature (missing .sig)", sums, []byte("")},
	}
	for _, c := range cases {
		if err := verifyChecksumsSig(c.sums, c.sig); err == nil {
			t.Errorf("%s: expected fail-closed, got nil", c.name)
		}
	}
}

// TestVerifyChecksumsSig_MalformedEmbeddedKey: a bad embedded key must fail closed, NOT
// panic (ed25519.Verify panics on a wrong-size public key) — AC11.
func TestVerifyChecksumsSig_MalformedEmbeddedKey(t *testing.T) {
	old := releaseVerifyKey
	releaseVerifyKey = ed25519.PublicKey([]byte("too-short"))
	t.Cleanup(func() { releaseVerifyKey = old })

	if err := verifyChecksumsSig([]byte("x"), b64(make([]byte, ed25519.SignatureSize))); err == nil {
		t.Error("a malformed embedded key must fail closed")
	}
}

func TestDecodeVerifyKey(t *testing.T) {
	if decodeVerifyKey("not base64!!") != nil {
		t.Error("malformed base64 → nil")
	}
	if decodeVerifyKey(base64.StdEncoding.EncodeToString(make([]byte, 5))) != nil {
		t.Error("wrong length → nil")
	}
	pub, _, _ := ed25519.GenerateKey(nil)
	if decodeVerifyKey(base64.StdEncoding.EncodeToString(pub)) == nil {
		t.Error("a valid key → non-nil")
	}
}

func signWithFreshKey(t *testing.T, msg []byte) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return b64(ed25519.Sign(priv, msg))
}
