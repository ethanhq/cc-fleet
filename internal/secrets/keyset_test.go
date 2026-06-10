package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// rotProvider returns a single-provider Config using the file backend with the
// given key_rotation strategy. (fileProvider in dispatch_test.go covers off.)
func rotProvider(name, ref, rotation string) *config.Config {
	return &config.Config{
		Version: config.SchemaVersion,
		Providers: map[string]*config.Provider{
			name: {
				Name:           name,
				BaseURL:        "https://api." + name + ".com/anthropic",
				DefaultModel:   name + "-latest",
				ModelsEndpoint: "https://api." + name + ".com/v1/models",
				SecretBackend:  "file",
				SecretRef:      ref,
				Enabled:        true,
				KeyRotation:    rotation,
				AddedAt:        time.Date(2026, 5, 26, 0, 0, 0, 0, time.UTC),
			},
		},
	}
}

// writeKeysJSON writes a well-formed <provider>.keys.json into the secrets dir.
func writeKeysJSON(t *testing.T, provider string, ks []KeyEntry) {
	t.Helper()
	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir secrets dir: %v", err)
	}
	data, err := json.MarshalIndent(ks, "", "  ")
	if err != nil {
		t.Fatalf("marshal keys.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, provider+".keys.json"), data, 0o600); err != nil {
		t.Fatalf("write keys.json: %v", err)
	}
}

// writeRawKeysJSON writes arbitrary (possibly malformed) bytes as a provider's
// keys.json — used by the corrupt-parse / leak-sentinel tests.
func writeRawKeysJSON(t *testing.T, provider, body string) {
	t.Helper()
	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir secrets dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, provider+".keys.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write raw keys.json: %v", err)
	}
}

// ---------------------------------------------------------------------------
// LoadKeySet
// ---------------------------------------------------------------------------

func TestLoadKeySet_LegacySingle(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	writeSecretFile(t, "deepseek.key", []byte("sk-legacy-123\n"))

	ks, err := LoadKeySet("deepseek")
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	if len(ks) != 1 {
		t.Fatalf("legacy load len = %d, want 1", len(ks))
	}
	if ks[0].Label != "key1" || ks[0].Key != "sk-legacy-123" || !ks[0].Enabled {
		t.Fatalf("legacy entry = %+v, want {key1, sk-legacy-123 (trimmed), true}", ks[0])
	}
}

func TestLoadKeySet_MultiAuthoritativeOverLegacy(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	// A legacy file ALSO exists — keys.json must win and the legacy file ignored.
	writeSecretFile(t, "deepseek.key", []byte("sk-legacy-should-be-ignored"))
	writeKeysJSON(t, "deepseek", []KeyEntry{
		{Label: "primary", Key: "sk-aaa-111", Enabled: true},
		{Label: "backup", Key: "sk-bbb-222", Enabled: false},
	})

	ks, err := LoadKeySet("deepseek")
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	if len(ks) != 2 {
		t.Fatalf("multi load len = %d, want 2", len(ks))
	}
	if ks[0].Key != "sk-aaa-111" || ks[1].Key != "sk-bbb-222" || ks[1].Enabled {
		t.Fatalf("multi entries = %+v, want keys.json order, backup disabled", ks)
	}
}

func TestLoadKeySet_MissingYieldsEmpty(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	// No legacy file, no keys.json.
	ks, err := LoadKeySet("deepseek")
	if err != nil {
		t.Fatalf("LoadKeySet on missing: %v (want nil err, empty set)", err)
	}
	if len(ks) != 0 {
		t.Fatalf("missing load len = %d, want 0", len(ks))
	}
}

// TestLoadKeySet_CorruptJSONNoKeyLeak: a keys.json that fails to parse must
// error WITHOUT echoing any key bytes into the message.
func TestLoadKeySet_CorruptJSONNoKeyLeak(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	const sentinel = "sk-SENTINEL-PLAINTEXT-must-never-leak-9999"
	// Valid-looking entry but missing the closing bracket -> parse failure.
	writeRawKeysJSON(t, "deepseek", `[{"label":"a","key":"`+sentinel+`","enabled":true}`)

	_, err := LoadKeySet("deepseek")
	if err == nil {
		t.Fatalf("LoadKeySet on corrupt keys.json: want error, got nil")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("parse error leaked key plaintext: %q", err.Error())
	}
}

// ---------------------------------------------------------------------------
// SaveKeySet (+ migration)
// ---------------------------------------------------------------------------

func TestSaveKeySet_MigratesLegacyAndIsAtomic0600(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	writeSecretFile(t, "deepseek.key", []byte("sk-legacy-seed"))

	// Migration flow: load legacy (seeds entry[0]) -> append -> save.
	ks, err := LoadKeySet("deepseek")
	if err != nil {
		t.Fatalf("LoadKeySet: %v", err)
	}
	ks = append(ks, KeyEntry{Label: "second", Key: "sk-second-key", Enabled: true})
	if err := SaveKeySet("deepseek", ks); err != nil {
		t.Fatalf("SaveKeySet: %v", err)
	}

	dir, _ := config.SecretsDir()
	kp := filepath.Join(dir, "deepseek.keys.json")
	info, err := os.Stat(kp)
	if err != nil {
		t.Fatalf("stat keys.json: %v", err)
	}
	// NTFS reports 0666; the 0600 contract is unix-only.
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm != 0o600 {
		t.Fatalf("keys.json perm = %o, want 600", perm)
	}

	// keys.json now authoritative; entry[0] is the migrated legacy key.
	reloaded, err := LoadKeySet("deepseek")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded) != 2 || reloaded[0].Key != "sk-legacy-seed" || reloaded[1].Key != "sk-second-key" {
		t.Fatalf("post-migration set = %+v, want [legacy-seed, second-key]", reloaded)
	}
	// The legacy file is left intact as a harmless backup.
	if _, err := os.Stat(filepath.Join(dir, "deepseek.key")); err != nil {
		t.Fatalf("legacy file should be preserved: %v", err)
	}
}

func TestSaveKeySet_EmptyWritesArrayNotNull(t *testing.T) {
	setupConfig(t, fileProvider("deepseek", "deepseek.key"))
	if err := SaveKeySet("deepseek", nil); err != nil {
		t.Fatalf("SaveKeySet(nil): %v", err)
	}
	dir, _ := config.SecretsDir()
	data, err := os.ReadFile(filepath.Join(dir, "deepseek.keys.json"))
	if err != nil {
		t.Fatalf("read keys.json: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "[]" {
		t.Fatalf("empty keyset persisted as %q, want %q", got, "[]")
	}
	ks, err := LoadKeySet("deepseek")
	if err != nil || len(ks) != 0 {
		t.Fatalf("reload empty: ks=%v err=%v, want empty,nil", ks, err)
	}
}

// ---------------------------------------------------------------------------
// MaskKey
// ---------------------------------------------------------------------------

func TestMaskKey(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"long", "sk-deepseek-238", "sk-…238"},
		{"exactly8", "abcdefgh", "abc…fgh"},
		{"short7", "abcdefg", "•••••••"},
		{"tiny", "ab", "•••"},
		{"empty", "", "•••"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := MaskKey(c.in); got != c.want {
				t.Fatalf("MaskKey(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestMaskKey_NeverRevealsMiddle(t *testing.T) {
	const key = "sk-supersecretmiddle-tail"
	masked := MaskKey(key)
	for _, leak := range []string{"supersecret", "secret", "middle"} {
		if strings.Contains(masked, leak) {
			t.Fatalf("MaskKey leaked %q in %q", leak, masked)
		}
	}
	if !strings.HasPrefix(masked, "sk-") || !strings.HasSuffix(masked, "ail") {
		t.Fatalf("MaskKey(%q) = %q, want sk-…ail shape", key, masked)
	}
}

// ---------------------------------------------------------------------------
// nextRoundRobinIndex (flock counter)
// ---------------------------------------------------------------------------

func TestNextRoundRobinIndex_CyclesAndWraps(t *testing.T) {
	setupConfig(t, fileProvider("rr", "rr.key"))
	var got []int
	for i := 0; i < 7; i++ {
		idx, err := nextRoundRobinIndex("rr", 3)
		if err != nil {
			t.Fatalf("nextRoundRobinIndex: %v", err)
		}
		got = append(got, idx)
	}
	want := []int{0, 1, 2, 0, 1, 2, 0}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("rr sequence = %v, want %v", got, want)
		}
	}
}

func TestNextRoundRobinIndex_CorruptCounterSelfHeals(t *testing.T) {
	setupConfig(t, fileProvider("rr", "rr.key"))
	dir, _ := config.SecretsDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "rr.rotation"), []byte("garbage"), 0o600); err != nil {
		t.Fatalf("write counter: %v", err)
	}
	idx, err := nextRoundRobinIndex("rr", 2)
	if err != nil {
		t.Fatalf("nextRoundRobinIndex: %v", err)
	}
	if idx != 0 {
		t.Fatalf("corrupt counter idx = %d, want 0 (self-heal)", idx)
	}
}

func TestNextRoundRobinIndex_AdaptsWhenSetShrinks(t *testing.T) {
	setupConfig(t, fileProvider("rr", "rr.key"))
	// Advance the monotonic counter to 3 with n=4 (idx 0,1,2,3 -> counter now 4).
	for i := 0; i < 4; i++ {
		if _, err := nextRoundRobinIndex("rr", 4); err != nil {
			t.Fatalf("warm-up: %v", err)
		}
	}
	// Now the enabled set shrinks to 2; the monotonic counter (4) maps via %2.
	idx, err := nextRoundRobinIndex("rr", 2)
	if err != nil {
		t.Fatalf("nextRoundRobinIndex: %v", err)
	}
	if idx != 0 { // 4 % 2
		t.Fatalf("idx after shrink = %d, want 0 (4%%2)", idx)
	}
}

// TestNextRoundRobinIndex_ConcurrentProcesses is the cross-process stress test:
// N separate OS processes each advance the same provider's rotation counter once.
// The flock in nextRoundRobinIndex must serialize every read-modify-write so the
// counter lands at exactly N — a lost increment (no lock / torn write) would
// leave it < N (real concurrent processes, not goroutines).
//
// The env-gated child branch performs one rotation step and exits, inheriting
// the parent's XDG_CONFIG_HOME so it targets the same <provider>.rotation file.
func TestNextRoundRobinIndex_ConcurrentProcesses(t *testing.T) {
	if os.Getenv("CCF_RR_CHILD") == "1" {
		n, _ := strconv.Atoi(os.Getenv("CCF_RR_N"))
		if _, err := nextRoundRobinIndex(os.Getenv("CCF_RR_PROVIDER"), n); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	setupConfig(t, fileProvider("conc", "conc.key"))
	dir, err := config.SecretsDir()
	if err != nil {
		t.Fatalf("SecretsDir: %v", err)
	}

	const N = 20
	var wg sync.WaitGroup
	errCh := make(chan error, N)
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestNextRoundRobinIndex_ConcurrentProcesses$")
			cmd.Env = append(os.Environ(), "CCF_RR_CHILD=1", "CCF_RR_PROVIDER=conc", "CCF_RR_N=4")
			if out, err := cmd.CombinedOutput(); err != nil {
				errCh <- fmt.Errorf("child rotate failed: %v\n%s", err, out)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	data, err := os.ReadFile(filepath.Join(dir, "conc.rotation"))
	if err != nil {
		t.Fatalf("read rotation counter: %v", err)
	}
	if got := parseCounter(data); got != N {
		t.Fatalf("counter = %d after %d concurrent rotations, want %d (lost increments => race)", got, N, N)
	}
}

// ---------------------------------------------------------------------------
// selectKey
// ---------------------------------------------------------------------------

func TestSelectKey_OffReturnsFirstEnabled(t *testing.T) {
	enabled := []KeyEntry{{Key: "first-aaa", Enabled: true}, {Key: "second-bbb", Enabled: true}}
	for _, rot := range []string{"", "off"} {
		got, err := selectKey("v", rot, enabled)
		if err != nil {
			t.Fatalf("selectKey(%q): %v", rot, err)
		}
		if string(got) != "first-aaa" {
			t.Fatalf("selectKey(%q) = %q, want first-aaa", rot, got)
		}
	}
}

func TestSelectKey_ZeroEnabledErrorsNoBytes(t *testing.T) {
	got, err := selectKey("v", "off", nil)
	if !errors.Is(err, ErrNoEnabledKey) {
		t.Fatalf("selectKey(empty) err = %v, want ErrNoEnabledKey", err)
	}
	if len(got) != 0 {
		t.Fatalf("selectKey(empty) returned %d bytes, want 0", len(got))
	}
}

func TestSelectKey_SingleEnabledIgnoresStrategy(t *testing.T) {
	setupConfig(t, fileProvider("solo", "solo.key")) // for the round_robin path's dir
	enabled := []KeyEntry{{Key: "only-key", Enabled: true}}
	for _, rot := range []string{"off", "round_robin", "random"} {
		got, err := selectKey("solo", rot, enabled)
		if err != nil {
			t.Fatalf("selectKey(%q): %v", rot, err)
		}
		if string(got) != "only-key" {
			t.Fatalf("selectKey(%q) = %q, want only-key", rot, got)
		}
	}
	// Single-key round_robin must NOT create a rotation counter (strategy moot).
	dir, _ := config.SecretsDir()
	if _, err := os.Stat(filepath.Join(dir, "solo.rotation")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("single-key round_robin created a counter file (err=%v)", err)
	}
}

func TestSelectKey_RoundRobinCycles(t *testing.T) {
	setupConfig(t, fileProvider("rr", "rr.key"))
	enabled := []KeyEntry{
		{Key: "key-a"}, {Key: "key-b"}, {Key: "key-c"},
	}
	for i := range enabled {
		enabled[i].Enabled = true
	}
	want := []string{"key-a", "key-b", "key-c", "key-a"}
	for i, w := range want {
		got, err := selectKey("rr", "round_robin", enabled)
		if err != nil {
			t.Fatalf("selectKey #%d: %v", i, err)
		}
		if string(got) != w {
			t.Fatalf("round_robin #%d = %q, want %q", i, got, w)
		}
	}
}

func TestSelectKey_RandomStaysInSet(t *testing.T) {
	enabled := []KeyEntry{{Key: "r0", Enabled: true}, {Key: "r1", Enabled: true}, {Key: "r2", Enabled: true}}
	allowed := map[string]bool{"r0": true, "r1": true, "r2": true}
	for i := 0; i < 50; i++ {
		got, err := selectKey("v", "random", enabled)
		if err != nil {
			t.Fatalf("selectKey random: %v", err)
		}
		if !allowed[string(got)] {
			t.Fatalf("random selected %q, outside the enabled set", got)
		}
	}
}

// ---------------------------------------------------------------------------
// IsMultiKey / RemoveKeySet
// ---------------------------------------------------------------------------

func TestIsMultiKey(t *testing.T) {
	setupConfig(t, fileProvider("dv", "dv.key"))
	if multi, err := IsMultiKey("dv"); err != nil || multi {
		t.Fatalf("IsMultiKey before keys.json = (%v,%v), want (false,nil)", multi, err)
	}
	writeKeysJSON(t, "dv", []KeyEntry{{Key: "k", Enabled: true}})
	if multi, err := IsMultiKey("dv"); err != nil || !multi {
		t.Fatalf("IsMultiKey after keys.json = (%v,%v), want (true,nil)", multi, err)
	}
}

func TestRemoveKeySet_DeletesBothAndIdempotent(t *testing.T) {
	setupConfig(t, fileProvider("dv", "dv.key"))
	writeKeysJSON(t, "dv", []KeyEntry{{Key: "k", Enabled: true}})
	if _, err := nextRoundRobinIndex("dv", 2); err != nil { // create the counter
		t.Fatalf("seed counter: %v", err)
	}
	dir, _ := config.SecretsDir()
	kp := filepath.Join(dir, "dv.keys.json")
	rp := filepath.Join(dir, "dv.rotation")

	if err := RemoveKeySet("dv"); err != nil {
		t.Fatalf("RemoveKeySet: %v", err)
	}
	for _, p := range []string{kp, rp} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s still present after RemoveKeySet (err=%v)", p, err)
		}
	}
	// Idempotent: a second call with nothing on disk is a no-op, not an error.
	if err := RemoveKeySet("dv"); err != nil {
		t.Fatalf("RemoveKeySet (idempotent) = %v, want nil", err)
	}
}

// ---------------------------------------------------------------------------
// Keyget integration (multi-key)
// ---------------------------------------------------------------------------

func TestKeyget_MultiKey_DisabledSkipped(t *testing.T) {
	setupConfig(t, fileProvider("ds", "ds.key"))
	writeKeysJSON(t, "ds", []KeyEntry{
		{Label: "dead", Key: "DISABLED-FIRST", Enabled: false},
		{Label: "live", Key: "LIVE-SECOND", Enabled: true},
	})
	// off rotation -> first ENABLED, i.e. the live key (the disabled one is
	// filtered out before indexing, so it can never be selected).
	got, err := Keyget("ds")
	if err != nil {
		t.Fatalf("Keyget: %v", err)
	}
	if string(got) != "LIVE-SECOND" {
		t.Fatalf("Keyget = %q, want LIVE-SECOND (disabled[0] must be skipped)", got)
	}
}

func TestKeyget_MultiKey_AllDisabledNoKey(t *testing.T) {
	setupConfig(t, fileProvider("ad", "ad.key"))
	writeKeysJSON(t, "ad", []KeyEntry{{Label: "x", Key: "NEVER", Enabled: false}})
	got, err := Keyget("ad")
	if !errors.Is(err, ErrNoEnabledKey) {
		t.Fatalf("Keyget all-disabled err = %v, want ErrNoEnabledKey", err)
	}
	if len(got) != 0 {
		t.Fatalf("Keyget all-disabled returned %d bytes, want 0", len(got))
	}
}

func TestKeyget_MultiKey_RoundRobin(t *testing.T) {
	setupConfig(t, rotProvider("rr", "rr.key", "round_robin"))
	writeKeysJSON(t, "rr", []KeyEntry{
		{Key: "RR-K0", Enabled: true},
		{Key: "RR-K1", Enabled: true},
	})
	want := []string{"RR-K0", "RR-K1", "RR-K0", "RR-K1"}
	for i, w := range want {
		got, err := Keyget("rr")
		if err != nil {
			t.Fatalf("Keyget #%d: %v", i, err)
		}
		if string(got) != w {
			t.Fatalf("Keyget round_robin #%d = %q, want %q", i, got, w)
		}
	}
}

func TestKeyget_MultiKey_CorruptNoLeak(t *testing.T) {
	setupConfig(t, fileProvider("cz", "cz.key"))
	const sentinel = "sk-LEAK-CANARY-plaintext-0000"
	writeRawKeysJSON(t, "cz", `[{"label":"a","key":"`+sentinel+`","enabled":true}`)

	got, err := Keyget("cz")
	if err == nil {
		t.Fatalf("Keyget on corrupt keys.json: want error, got nil")
	}
	if strings.Contains(err.Error(), sentinel) || strings.Contains(err.Error(), "CANARY") {
		t.Fatalf("Keyget error leaked key plaintext: %q", err.Error())
	}
	if len(got) != 0 {
		t.Fatalf("Keyget on corrupt keys.json returned %d bytes, want 0", len(got))
	}
}

// TestLoadKeySet_RejectsPathTraversal guards the keyset path builders against a
// provider name that would escape SecretsDir (defense-in-depth). Real callers
// pass regex-validated registered names; this proves a direct caller
// can't turn "../x" into a read outside the secrets dir.
func TestLoadKeySet_RejectsPathTraversal(t *testing.T) {
	setupConfig(t, fileProvider("ok", "ok.key"))
	for _, bad := range []string{"", ".", "..", "../../etc/passwd", "a/b", `a\b`, "foo/.."} {
		if _, err := LoadKeySet(bad); err == nil {
			t.Errorf("LoadKeySet(%q): want error for unsafe provider name, got nil", bad)
		}
		if err := SaveKeySet(bad, []KeyEntry{{Key: "x", Enabled: true}}); err == nil {
			t.Errorf("SaveKeySet(%q): want error for unsafe provider name, got nil", bad)
		}
	}
}

// TestSafeRef pins the file-backend secret_ref guard: only a flat filename
// inside the secrets dir is accepted, and the error never echoes the ref
// (no-leak posture).
func TestSafeRef(t *testing.T) {
	for _, bad := range []string{"", ".", "..", "/etc/passwd", "../x", "a/b", `a\b`, "foo/..", "x..y"} {
		if err := SafeRef(bad); err == nil {
			t.Errorf("SafeRef(%q): want error, got nil", bad)
		}
	}
	for _, ok := range []string{"deepseek.key", "glm.keys.json", "provider.rotation", "a.b.c"} {
		if err := SafeRef(ok); err != nil {
			t.Errorf("SafeRef(%q): want nil, got %v", ok, err)
		}
	}
	if err := SafeRef("../SENTINEL-REF-VALUE"); err == nil || strings.Contains(err.Error(), "SENTINEL") {
		t.Fatalf("SafeRef error must reject and not echo the ref: %v", err)
	}
}

// TestLoadKeySet_RejectsUnsafeRef: a hand-edited providers.toml could point a
// file-backend secret_ref outside SecretsDir; LoadKeySet's legacy path must
// refuse it instead of reading through
// filepath.Join. (config.Validate only presence-checks secret_ref, so the bad
// ref does persist — this is the last line of defense.)
func TestLoadKeySet_RejectsUnsafeRef(t *testing.T) {
	setupConfig(t, fileProvider("v", "../../etc/shadow"))
	if _, err := LoadKeySet("v"); err == nil {
		t.Fatal("LoadKeySet with traversal secret_ref: want error, got nil")
	}
}
