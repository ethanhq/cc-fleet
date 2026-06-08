package codexproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// Serve runs a proxy daemon for protocol on port: it binds the loopback port,
// builds the protocol's upstream, records its state, serves the conversion
// handler, and self-exits once no matching worker and no launch lease remain
// (re-checked under the port's proxy lock; a scan error keeps it alive).
func Serve(port int, protocol, upstreamURL string) error {
	if protocol == "" {
		protocol = config.ProtocolCodexOAuth
	}
	up, err := buildUpstream(protocol, upstreamURL)
	if err != nil {
		return err
	}
	// codex gates /v1/messages on the handshake secret (its OAuth bearer lives
	// only here); an openai-* daemon takes the real key per request, so it needs
	// none. Read it lock-free: EnsureDaemon created it under the global secret
	// lock before spawning this daemon. A manual `serve` falls back to creating it.
	var secret string
	if protocol == config.ProtocolCodexOAuth {
		if secret, err = loadOrCreateSecret(); err != nil {
			return err
		}
	}
	srv := newServer(up, protocol, secret)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
	}
	start, _ := procintrospect.ProcStart(os.Getpid())
	if err := writeState(proxyState{Port: port, PID: os.Getpid(), ProcStart: start, Protocol: protocol, UpstreamURL: upstreamURL}); err != nil {
		ln.Close()
		return err
	}

	httpSrv := &http.Server{Handler: srv.handler()}
	go func() {
		for {
			time.Sleep(60 * time.Second)
			if maybeShutdown(port, srv, httpSrv) {
				return
			}
		}
	}()
	if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	clearStateIfOwner(port)
	return nil
}

// maybeShutdown decides under the port's proxy lock whether the daemon may stop,
// and if so shuts the listener (releasing the port) and clears its own state — all
// WITHIN the lock so a concurrent EnsureDaemon never observes a half-torn-down
// daemon (state gone but the port still held) nor has its own fresh state deleted
// by us. It may stop only when no unexpired launch lease and no live worker remain
// and it has been idle past the grace period; any introspection uncertainty (-1
// workers) keeps it alive.
func maybeShutdown(port int, srv *server, httpSrv *http.Server) bool {
	stopped := false
	_ = withProxyLock(port, func() error {
		if activeLeases(port) > 0 {
			return nil
		}
		if workers := liveWorkers(port); workers != 0 {
			return nil // >0 live, or -1 unknown -> stay alive
		}
		if time.Since(time.Unix(0, srv.lastActivity.Load())) < idleGrace {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(ctx)
		cancel()
		clearStateIfOwner(port)
		stopped = true
		return nil
	})
	return stopped
}

func clearState(port int) {
	if p, err := statePath(port); err == nil {
		os.Remove(p)
	}
}

// clearStateIfOwner removes the state file only when it still describes THIS
// process, so a shutting-down daemon never deletes a replacement's fresh state.
func clearStateIfOwner(port int) {
	if st, err := readState(port); err == nil && st.PID != os.Getpid() {
		return
	}
	clearState(port)
}

// StopDaemon stops every running proxy daemon (explicit `cc-fleet codex-proxy
// stop` / logout / uninstall). Each port is stopped under its own proxy lock.
func StopDaemon() error {
	for _, st := range listStates() {
		port := st.Port
		_ = withProxyLock(port, func() error {
			cur, err := readState(port)
			if err != nil {
				return nil // already gone
			}
			if cur.PID > 0 && pidAlive(cur.PID, cur.ProcStart) {
				if proc, e := os.FindProcess(cur.PID); e == nil {
					_ = proc.Signal(os.Interrupt)
				}
			}
			clearState(port)
			return nil
		})
	}
	return nil
}

// DaemonInfo describes a running proxy daemon for `codex-proxy status`.
type DaemonInfo struct {
	Port     int
	Protocol string
}

// RunningDaemons lists every live proxy daemon.
func RunningDaemons() []DaemonInfo {
	var out []DaemonInfo
	for _, st := range listStates() {
		if st.PID > 0 && pidAlive(st.PID, st.ProcStart) {
			out = append(out, DaemonInfo{Port: st.Port, Protocol: st.Protocol})
		}
	}
	return out
}

// Purge stops every daemon and removes all proxy state (per-port state files,
// lease dirs, lock files, the handshake secret, the secret + token locks) for
// uninstall. The login token chain is a credential: kept when keepToken (uninstall's
// KeepSecrets), else removed. Best-effort — failures land in kept with the reason,
// never abort.
func Purge(keepToken bool) (removed, kept []string) {
	_ = StopDaemon() // clears each per-port state file

	rm := func(p string) {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			kept = append(kept, fmt.Sprintf("%s (remove failed: %v)", p, err))
			return
		}
		removed = append(removed, p)
	}
	if dir, err := config.ConfigDir(); err == nil {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			name := e.Name()
			full := filepath.Join(dir, name)
			switch {
			case strings.HasPrefix(name, "codex-proxy-leases-"):
				if rerr := os.RemoveAll(full); rerr != nil {
					kept = append(kept, fmt.Sprintf("%s (rm -rf failed: %v)", full, rerr))
				} else {
					removed = append(removed, full)
				}
			case strings.HasPrefix(name, "codex-proxy-") && strings.HasSuffix(name, ".json"):
				rm(full)
			case strings.HasPrefix(name, ".cc-fleet-codex-proxy-") && strings.HasSuffix(name, ".lock"):
				rm(full)
			case name == "codex-proxy-secret",
				name == ".cc-fleet-codex-secret.lock",
				name == ".cc-fleet-codex-token.lock":
				rm(full)
			}
		}
	}

	tokenPath, terr := storePath()
	if terr == nil {
		if keepToken {
			if _, err := os.Stat(tokenPath); err == nil {
				kept = append(kept, tokenPath)
			}
		} else {
			rm(tokenPath)
		}
	}
	return removed, kept
}
