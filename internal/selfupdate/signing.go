package selfupdate

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
)

// releaseSigningKeyB64 is the base64 Ed25519 PUBLIC key the release pipeline signs
// checksums.txt with (see tools/sign-checksums). The matching private seed is the
// CCF_RELEASE_SIGNING_KEY release secret; signing-preflight.sh fails the release unless
// this constant equals the key derived from that secret.
const releaseSigningKeyB64 = "dAbMy8Omb0En+n0xZNGTjKsNHDdwBipwqy+jHXGpZjw="

// releaseVerifyKey is the decoded public key, or nil if releaseSigningKeyB64 is
// malformed — verifyChecksumsSig then fails closed rather than letting ed25519.Verify
// panic on a wrong-size key.
var releaseVerifyKey = decodeVerifyKey(releaseSigningKeyB64)

// decodeVerifyKey returns the Ed25519 public key for a base64 string, or nil if it is
// not exactly ed25519.PublicKeySize bytes.
func decodeVerifyKey(b64 string) ed25519.PublicKey {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(raw)
}

// verifyChecksumsSig verifies a base64 Ed25519 detached signature (sigB64) over the
// EXACT bytes of checksums.txt (sums) against the embedded release public key. It is
// the trust anchor the self-update path checks before trusting any sha256: it fails
// closed on a malformed embedded key, a malformed/wrong-length signature, or a
// verification mismatch, and never panics.
func verifyChecksumsSig(sums, sigB64 []byte) error {
	if len(releaseVerifyKey) != ed25519.PublicKeySize {
		return errors.New("release signing key unavailable (malformed embedded public key)")
	}
	sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigB64)))
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("signature is %d bytes, want %d", len(sig), ed25519.SignatureSize)
	}
	if !ed25519.Verify(releaseVerifyKey, sums, sig) {
		return errors.New("checksums.txt signature does not match the release key")
	}
	return nil
}
