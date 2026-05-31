package version

import "testing"

// A release / curl / npm binary is built with -ldflags -X, so a stamped
// Version must win over any build-info fallback.
func TestResolve_StampedVersionWins(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = "v1.2.3"
	if got := Resolve(); got != "v1.2.3" {
		t.Fatalf("Resolve() = %q, want the stamped v1.2.3", got)
	}
}

// Without a stamp, a local build's build-info version is "", "(devel)", or a
// pseudo-version — Resolve must fall back to the readable dev default.
func TestResolve_UnstampedFallsBackToDevDefault(t *testing.T) {
	orig := Version
	t.Cleanup(func() { Version = orig })

	Version = devDefault
	if got := Resolve(); got != devDefault {
		t.Fatalf("Resolve() = %q, want the dev default %q for an unstamped build", got, devDefault)
	}
}
