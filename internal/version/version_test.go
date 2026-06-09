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

func TestIsRelease(t *testing.T) {
	releases := []string{"0.1.6", "v0.1.6", "1.2.3", "v10.20.30", " v0.1.6 "}
	for _, v := range releases {
		if !IsRelease(v) {
			t.Errorf("IsRelease(%q) = false, want true", v)
		}
	}
	nonReleases := []string{"0.1.0-dev", "v0.1.7-0.20260609133342-c27032ba21ef", "(devel)", "1.2", "1.2.3.4", "v1.2.x", ""}
	for _, v := range nonReleases {
		if IsRelease(v) {
			t.Errorf("IsRelease(%q) = true, want false", v)
		}
	}
}

func TestNewer(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.1.8", "v0.1.6", true},
		{"0.1.8", "v0.1.6", true}, // mixed v-prefix
		{"v0.2.0", "v0.1.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.1.6", "v0.1.6", false},    // equal is not newer
		{"v0.1.6", "v0.1.8", false},    // older
		{"0.1.0-dev", "v0.1.6", false}, // non-release current never "newer"
		{"v0.1.8", "0.1.0-dev", false}, // non-release base is not comparable
	}
	for _, c := range cases {
		if got := Newer(c.a, c.b); got != c.want {
			t.Errorf("Newer(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	if Normalize("v0.1.6") != "0.1.6" || Normalize("0.1.6") != "0.1.6" {
		t.Fatalf("Normalize did not strip the leading v consistently")
	}
}
