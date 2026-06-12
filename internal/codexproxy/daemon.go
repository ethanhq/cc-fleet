package codexproxy

import (
	"context"
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
	"github.com/ethanhq/cc-fleet/internal/diag"
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

// Daemon state is per-port: a state file, a start/exit lock, and a lease dir keyed
// by port, so a codex daemon and an openai daemon (or two openai daemons) coexist
// without overwriting each other. The handshake secret + its create-once lock stay
// GLOBAL (one per install); the token lock is per credential (see tokenstore.go).
func statePath(port int) (string, error) {
	return joinConfig(fmt.Sprintf("codex-proxy-%d.json", port))
}
func lockPath(port int) (string, error) {
	return joinConfig(fmt.Sprintf(".cc-fleet-codex-proxy-%d.lock", port))
}
func leasesDir(port int) (string, error) {
	return joinConfig(fmt.Sprintf("codex-proxy-leases-%d", port))
}
func secretPath() (string, error)     { return joinConfig("codex-proxy-secret") }
func secretLockPath() (string, error) { return joinConfig(".cc-fleet-codex-secret.lock") }

func joinConfig(name string) (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// proxyState is the persisted daemon descriptor: pid + procStart guard against PID
// reuse; the bound port; and the identity (protocol + upstream URL + codex credential
// ref) a reuse check matches against so a daemon left from a different provider — or a
// different codex account on a recycled port — is never reused. Credential is omitempty
// so a state file written before multi-credential reads back as the default credential.
type proxyState struct {
	Port        int    `json:"port"`
	PID         int    `json:"pid"`
	ProcStart   string `json:"proc_start"`
	Protocol    string `json:"protocol"`
	UpstreamURL string `json:"upstream_url,omitempty"`
	Credential  string `json:"credential,omitempty"`
}

// withProxyLock serializes a single port's daemon start / exit decisions. It is a
// standalone flock scope, held with none of the others (providers / team / server /
// run / secret / token), so it cannot form a lock-order cycle.
func withProxyLock(port int, fn func() error) error {
	p, err := lockPath(port)
	if err != nil {
		return err
	}
	return config.WithFlock(p, fn)
}

// withSecretLock serializes the handshake secret's create-once. It is global (the
// secret is per-install), so two first-time daemons on different ports never
// race-overwrite it (which would desync keyget from a running daemon). Standalone
// — acquired and released before the per-port proxy lock in EnsureDaemon, never
// nested with it, so no ordering cycle.
func withSecretLock(fn func() error) error {
	p, err := secretLockPath()
	if err != nil {
		return err
	}
	return config.WithFlock(p, fn)
}

// SecretForKeyget returns the stable loopback handshake secret, creating it once
// under the global secret lock. keyget hands this (not the upstream token) to the
// claude process.
func SecretForKeyget() (string, error) {
	var secret string
	err := withSecretLock(func() error {
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

// PortFromBaseURL extracts the loopback port from a daemon-backed provider's
// base_url, validating it is a plain loopback http URL first (config.ParseLoopbackURL
// is the shared definition, also used at config load/validate time).
func PortFromBaseURL(baseURL string) (int, error) {
	u, err := config.ParseLoopbackURL(baseURL)
	if err != nil {
		return 0, fmt.Errorf("base_url %w", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port <= 0 {
		return 0, fmt.Errorf("base_url %q has no valid port", baseURL)
	}
	return port, nil
}

// EnsureForProviderName loads the named provider and ensures the proxy for it. The
// workflow engine uses this (it has only the provider name) to ensure the daemon
// before minting a queued leaf. An unknown provider is a no-op here — the leaf's own
// path surfaces it.
func EnsureForProviderName(name string) error {
	// The native leaf rides no daemon and must not touch provider config at
	// all — neither a malformed providers.toml nor a stale hand-made row may
	// gate or side-effect it.
	if name == config.ReservedNativeProvider {
		return nil
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return EnsureForProvider(cfg.Providers[name], nil)
}

// EnsureForProvider ensures the proxy daemon is up for a daemon-backed provider (a
// no-op for an Anthropic-native provider). Call it after the fingerprint gate and
// before the profile write. Keys on the normalized protocol so a codex row
// predating the protocol field (codex-oauth backend, no protocol) is recognized.
// dg is the --verbose step-trace sink (nil = silent).
func EnsureForProvider(v *config.Provider, dg *diag.Logger) error {
	if v == nil || !v.DaemonBacked() {
		return nil
	}
	port, err := PortFromBaseURL(v.BaseURL)
	if err != nil {
		return err
	}
	// codex also serves its models_endpoint from the daemon, and a probe sends the
	// handshake secret there, so it too must stay loopback.
	ref := ""
	if v.EffectiveProtocol() == config.ProtocolCodexOAuth {
		ref = v.SecretRef // the per-provider credential id (a codex row always has one)
		if v.ModelsEndpoint != "" {
			if _, err := config.ParseLoopbackURL(v.ModelsEndpoint); err != nil {
				return fmt.Errorf("codex models_endpoint %w", err)
			}
		}
	}
	return EnsureDaemon(port, v.EffectiveProtocol(), v.UpstreamURL, ref, dg)
}

// EnsureDaemon makes the proxy daemon for (port, protocol, upstreamURL) reachable,
// lazily and single-flight under that port's proxy lock, and registers a launch
// lease that keeps it alive across the window before the launched claude is
// visible. A live daemon is reused only if its persisted identity matches; a
// mismatch is torn down and restarted. Slot it after the fingerprint gate and
// before the profile-write side effect.
func EnsureDaemon(port int, protocol, upstreamURL, ref string, dg *diag.Logger) error {
	if protocol == config.ProtocolCodexOAuth {
		if err := withSecretLock(func() error { _, e := loadOrCreateSecret(); return e }); err != nil {
			return err
		}
	}
	return withProxyLock(port, func() error {
		if !healthy(port, protocol, upstreamURL, ref) {
			dg.Logf("daemon: port %d not serving this identity — (re)starting", port)
			stopStaleDaemon(port)
			if err := startDetached(port, protocol, upstreamURL, ref); err != nil {
				return err
			}
			dg.Logf("daemon: started detached on port %d", port)
			if err := waitReady(port); err != nil {
				return err
			}
			dg.Logf("daemon: ready on port %d", port)
		} else {
			dg.Logf("daemon: reusing live daemon on port %d", port)
		}
		return registerLease(port)
	})
}

// healthy reports whether a live daemon on port already serves this exact identity.
// A dead pid, a different protocol/upstream (port reuse or an edited provider), or an
// unresponsive port all return false. Readiness is the upstream-independent
// /healthz: an openai daemon's /v1/models returns an empty list (its models come
// from the real upstream), so it is no readiness signal.
func healthy(port int, protocol, upstreamURL, ref string) bool {
	st, err := readState(port)
	if err != nil || st.Port != port || st.PID <= 0 || !pidAlive(st.PID, st.ProcStart) {
		return false
	}
	if st.Protocol != protocol || st.UpstreamURL != upstreamURL {
		return false
	}
	// A recycled port (remove+add landing on the same port) must never reuse a codex
	// daemon bound to a different credential — that would serve the wrong account.
	if protocol == config.ProtocolCodexOAuth && !sameCredential(st.Credential, ref) {
		return false
	}
	return portResponds(port)
}

// stopStaleDaemon stops a live daemon whose identity no longer matches and waits
// briefly for it to free the port, so EnsureDaemon can rebind it. A no-op when no
// daemon (or a dead one) holds the port. loadSecretOnly takes no flock, so reading
// it here (inside EnsureDaemon's proxy lock) cannot deadlock a concurrent ensure.
func stopStaleDaemon(port int) {
	st, err := readState(port)
	if err != nil {
		return
	}
	if st.PID > 0 && pidAlive(st.PID, st.ProcStart) {
		stopViaShutdown(port, st, loadSecretOnly())
		for i := 0; i < 20 && whoHoldsPort(port); i++ {
			time.Sleep(50 * time.Millisecond)
		}
	}
	clearState(port)
}

func portResponds(port int) bool {
	c := &http.Client{Timeout: 1500 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func startDetached(port int, protocol, upstreamURL, ref string) error {
	if held := whoHoldsPort(port); held {
		return fmt.Errorf("port %d is held by another process; free it or set a different base_url port", port)
	}
	bin, err := os.Executable()
	if err != nil {
		return err
	}
	args := []string{"codex-proxy", "serve", "--port", strconv.Itoa(port), "--protocol", protocol}
	if upstreamURL != "" {
		args = append(args, "--upstream-url", upstreamURL)
	}
	// The codex credential the daemon binds to (omitted for the default / non-codex,
	// which keeps the launch argv byte-stable for existing single-codex installs).
	if !isDefaultCredential(ref) {
		args = append(args, "--credential", ref)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout, cmd.Stderr = nil, nil
	// Scrub the launcher's creds + nested-CC/teams markers from the long-lived
	// daemon (codex authenticates via its own OAuth chain; an openai daemon takes
	// the key per request, never from the lead's env).
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

func readState(port int) (proxyState, error) {
	var st proxyState
	p, err := statePath(port)
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
	p, err := statePath(st.Port)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(st, "", "  ")
	return fileutil.AtomicWrite(p, b, 0o600)
}

// listStates returns every persisted per-port daemon descriptor.
func listStates() []proxyState {
	dir, err := config.ConfigDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []proxyState
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "codex-proxy-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var st proxyState
		if json.Unmarshal(b, &st) == nil && st.Port > 0 {
			out = append(out, st)
		}
	}
	return out
}

func pidAlive(pid int, procStart string) bool {
	if start, ok := procintrospect.ProcStart(pid); ok {
		return start == procStart
	}
	return false
}

// registerLease writes a short-TTL lease so the daemon won't exit during the window
// between ensureDaemon returning and the launched claude appearing in the table.
func registerLease(port int) error {
	dir, err := leasesDir(port)
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

// KeepAlive holds a daemon-backed provider's daemon up for the lifetime of ctx by
// renewing the per-port launch lease on a ticker (the lane-0 interactive run has no
// job file, so its resident parent renews the lease while its claude child runs). It
// blocks until ctx is done. A nil or non-daemon-backed provider returns immediately.
func KeepAlive(ctx context.Context, v *config.Provider) {
	if v == nil || !v.DaemonBacked() {
		return
	}
	port, err := PortFromBaseURL(v.BaseURL)
	if err != nil {
		return
	}
	ticker := time.NewTicker(leaseTTL / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = registerLease(port)
		}
	}
}

// activeLeases counts unexpired leases for a port and prunes expired ones.
func activeLeases(port int) int {
	dir, err := leasesDir(port)
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

// liveWorkers counts running claude workers bound to this proxy's port; a return of
// -1 ("unknown") keeps the daemon alive. The count source is platform-split: unix
// scans process argv for the --settings profile, windows scans cc-fleet's own job
// store (argv is unreadable there). See liveworkers_{unix,windows}.go.
