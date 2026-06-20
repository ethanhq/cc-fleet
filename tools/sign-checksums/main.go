// Command sign-checksums is the release-side counterpart to the self-update signature
// verifier: it signs a release artifact (checksums.txt) with the project's Ed25519 release
// key, derives the public key for the release key-match preflight, or generates a fresh
// keypair. It is invoked by goreleaser's signs: step and the release workflow; it is never
// part of the shipped cc-fleet binary.
//
//	sign-checksums <artifact> <signature-out>   sign <artifact>, write base64 sig to <signature-out>
//	sign-checksums -pubkey                       print the base64 public key for $CCF_RELEASE_SIGNING_KEY
//	sign-checksums -genkey                        print a fresh SEED= / PUBKEY= pair
//
// The signing key is the base64 32-byte Ed25519 seed in $CCF_RELEASE_SIGNING_KEY.
package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
)

const seedEnv = "CCF_RELEASE_SIGNING_KEY"

func main() {
	pubkey := flag.Bool("pubkey", false, "print the base64 public key derived from $"+seedEnv)
	genkey := flag.Bool("genkey", false, "generate a fresh keypair and print SEED= / PUBKEY=")
	flag.Parse()

	if *genkey {
		pub, priv, err := ed25519.GenerateKey(nil)
		if err != nil {
			fail(err)
		}
		fmt.Printf("SEED=%s\nPUBKEY=%s\n",
			base64.StdEncoding.EncodeToString(priv.Seed()),
			base64.StdEncoding.EncodeToString(pub))
		return
	}

	seed, err := loadSeed()
	if err != nil {
		fail(err)
	}
	priv := ed25519.NewKeyFromSeed(seed)

	if *pubkey {
		fmt.Println(base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey)))
		return
	}

	args := flag.Args()
	if len(args) != 2 {
		fail(fmt.Errorf("usage: sign-checksums <artifact> <signature-out> (or -pubkey / -genkey)"))
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fail(err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(priv, data))
	if err := os.WriteFile(args[1], []byte(sig+"\n"), 0o644); err != nil {
		fail(err)
	}
}

// loadSeed reads the base64 32-byte Ed25519 seed from $CCF_RELEASE_SIGNING_KEY.
func loadSeed() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv(seedEnv))
	if raw == "" {
		return nil, fmt.Errorf("%s is empty", seedEnv)
	}
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", seedEnv, err)
	}
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("%s must decode to %d bytes, got %d", seedEnv, ed25519.SeedSize, len(seed))
	}
	return seed, nil
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "sign-checksums:", err)
	os.Exit(1)
}
