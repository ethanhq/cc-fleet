package codexproxy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// Daemon timing. leaseTTL must comfortably exceed the gap between ensureDaemon
// returning and the launched claude appearing in the process table.
const (
	leaseTTL     = 90 * time.Second
	idleGrace    = 5 * time.Minute
	readyTimeout = 10 * time.Second
)

func stateDir() (string, error)   { return config.ConfigDir() }
func statePath() (string, error)  { return joinConfig("codex-proxy.json") }
func secretPath() (string, error) { return joinConfig("codex-proxy-secret") }
func lockPath() (string, error)   { return joinConfig(".cc-fleet-codex-proxy.lock") }
func leasesDir() (string, error)  { return joinConfig("codex-proxy-leases") }

func joinConfig(name string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// proxyState is the persisted daemon descriptor (pid + procStart guard against PID
// reuse; the port the daemon is bound to).
type proxyState struct {
	Port      int    `json:"port"`
	PID       int    `json:"pid"`
	ProcStart string `json:"proc_start"`
}

// withProxyLock serializes daemon start / exit decisions. It is the FIFTH flock
// scope and is held with NONE of the other four (vendors / team / server / run),
// so it cannot form a lock-order cycle.
func withProxyLock(fn func() error) error {
	p, err := lockPath()
	if err != nil {
		return err
	}
	return config.WithFlock(p, fn)
}

// SecretForKeyget returns the stable loopback handshake secret, creating it once.
// keyget hands this (not the upstream token) to the claude process.
func SecretForKeyget() (string, error) {
	var secret string
	err := withProxyLock(func() error {
		s, e := loadOrCreateSecret()
		secret = s
		return e
	})
	return secret, err
}

func loadOrCreateSecret() (string, error) {
	p, err := secretPath()
	if err != nil {
		return "", err
	}
	if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
		return strings.TrimSpace(string(b)), nil
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	secret := hex.EncodeToString(buf)
	if err := fileutil.AtomicWrite(p, []byte(secret), 0o600); err != nil {
		return "", err
	}
	return secret, nil
}

// PortFromBaseURL extracts the loopback port from a codex vendor's base_url,
// validating it is a plain loopback http URL first (config.ParseLoopbackURL is
// the shared definition, also used at config load/validate time).
func PortFromBaseURL(baseURL string) (int, error) {
	u, err := config.ParseLoopbackURL(baseURL)
	if err != nil {
		return 0, fmt.Errorf("codex base_url %w", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("codex base_url %q has no valid port", baseURL)
	}
	return port, nil
}

// EnsureForVendorName loads the named vendor and ensures the proxy for it. The
// workflow engine uses this (it has only the vendor name) to ensure the daemon
// before minting a queued leaf. An unknown vendor is a no-op here — the leaf's own
// path surfaces it.
func EnsureForVendorName(name string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return EnsureForVendor(cfg.Vendors[name])
}

// EnsureForVendor ensures the proxy daemon is up for a codex provider (a no-op for
// any other vendor). Call it after the fingerprint gate and before the profile write.
func EnsureForVendor(v *config.Vendor) error {
	if v == nil || v.SecretBackend != SecretBackend {
		return nil
	}
	// models_endpoint must also stay loopback — a probe sends the handshake secret
	// there, so a remote one would leak it off-host.
	if v.ModelsEndpoint != "" {
		if _, err := config.ParseLoopbackURL(v.ModelsEndpoint); err != nil {
			return fmt.Errorf("codex models_endpoint %w", err)
		}
	}
	port, err := PortFromBaseURL(v.BaseURL)
	if err != nil {
		return err
	}
	return EnsureDaemon(port)
}

// EnsureDaemon makes the codex proxy daemon reachable on port, lazily and
// single-flight under the proxy lock, and registers a launch lease that keeps the
// daemon alive across the window before the launched claude is visible. Slot it
// after the fingerprint gate and before the profile-write side effect.
func EnsureDaemon(port int) error {
	return withProxyLock(func() error {
		if _, err := loadOrCreateSecret(); err != nil {
			return err
		}
		if !healthy(port) {
			if err := startDetached(port); err != nil {
				return err
			}
			if err := waitReady(port); err != nil {
				return err
			}
		}
		return registerLease()
	})
}

func healthy(port int) bool {
	st, err := readState()
	if err != nil || st.Port != port || st.PID <= 0 || !pidAlive(st.PID, st.ProcStart) {
		return false
	}
	return portResponds(port)
}

func portResponds(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func startDetached(port int) error {
	if held := whoHoldsPort(port); held {
		return fmt.Errorf("port %d is held by another process; free it or set a different codex base_url port", port)
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "codex-proxy", "serve", "--port", strconv.Itoa(port))
	cmd.Stdout, cmd.Stderr = nil, nil
	// Scrub the launcher's creds + nested-CC/teams markers from the long-lived
	// daemon (it authenticates via its own OAuth chain, never the lead's env).
	cmd.Env = childenv.Clean(os.Environ())
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	// The daemon owns its own lifecycle (self-exits when idle), so release the
	// handle rather than leak it or leave a zombie when it eventually exits.
	return cmd.Process.Release()
}

// whoHoldsPort reports whether the loopback port is already bound (by anyone). A
// just-started daemon of ours will be caught by healthy()/waitReady() instead.
func whoHoldsPort(port int) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return true
	}
	ln.Close()
	return false
}

func waitReady(port int) error {
	deadline := time.Now().Add(readyTimeout)
	for time.Now().Before(deadline) {
		if portResponds(port) {
			return nil
		}
		time.Sleep(150 * time.Millisecond)
	}
	return fmt.Errorf("codex proxy did not become ready on port %d", port)
}

func readState() (proxyState, error) {
	var st proxyState
	p, err := statePath()
	if err != nil {
		return st, err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(b, &st)
}

func writeState(st proxyState) error {
	p, err := statePath()
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return fileutil.AtomicWrite(p, b, 0o600)
}

func pidAlive(pid int, procStart string) bool {
	if start, ok := procintrospect.ProcStart(pid); ok {
		return start == procStart
	}
	return false
}

// registerLease writes a short-TTL lease so the daemon won't exit during the window
// between ensureDaemon returning and the launched claude appearing in the table.
func registerLease() error {
	dir, err := leasesDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	name := fmt.Sprintf("%d-%s", os.Getpid(), hex.EncodeToString(buf))
	expires := time.Now().Add(leaseTTL).UnixNano()
	return fileutil.AtomicWrite(filepath.Join(dir, name), []byte(strconv.FormatInt(expires, 10)), 0o600)
}

// activeLeases counts unexpired leases and prunes expired ones.
func activeLeases() int {
	dir, err := leasesDir()
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	now := time.Now().UnixNano()
	active := 0
	for _, e := range entries {
		full := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(full)
		if err != nil {
			continue
		}
		exp, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
		if err != nil || exp < now {
			os.Remove(full)
			continue
		}
		active++
	}
	return active
}

// liveCodexWorkers counts running claude processes whose --settings profile points
// at this proxy's port. A scan error returns -1 ("unknown" -> daemon stays alive).
func liveCodexWorkers(port int) int {
	table, err := procintrospect.ProcessTable()
	if err != nil {
		return -1
	}
	n := 0
	for _, p := range table {
		if profileTargetsPort(p.Argv, port) {
			n++
		}
	}
	return n
}

func profileTargetsPort(argv []string, port int) bool {
	settings := settingsPath(argv)
	if settings == "" {
		return false
	}
	b, err := os.ReadFile(settings)
	if err != nil {
		return false
	}
	var prof struct {
		Env map[string]string `json:"env"`
	}
	if json.Unmarshal(b, &prof) != nil {
		return false
	}
	return strings.Contains(prof.Env["ANTHROPIC_BASE_URL"], fmt.Sprintf(":%d", port))
}

func settingsPath(argv []string) string {
	for i, a := range argv {
		if a == "--settings" && i+1 < len(argv) {
			return argv[i+1]
		}
		if strings.HasPrefix(a, "--settings=") {
			return strings.TrimPrefix(a, "--settings=")
		}
	}
	return ""
}
