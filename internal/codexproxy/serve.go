package codexproxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// Serve runs the codex proxy daemon: it binds the loopback port, records its state,
// serves the conversion handler, and self-exits once no codex worker and no launch
// lease remain (re-checked under the proxy lock; a scan error keeps it alive).
func Serve(port int) error {
	// Read the handshake secret lock-free: EnsureDaemon creates it under the proxy
	// lock BEFORE spawning this daemon and holds that lock across waitReady, so
	// taking it here would deadlock. A manual `codex-proxy serve` (no EnsureDaemon)
	// falls back to creating it directly.
	secret, err := loadOrCreateSecret()
	if err != nil {
		return err
	}
	source, err := newOwnStore()
	if err != nil {
		return err
	}
	srv := newServer(source, secret)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("bind 127.0.0.1:%d: %w", port, err)
	}
	start, _ := procintrospect.ProcStart(os.Getpid())
	if err := writeState(proxyState{Port: port, PID: os.Getpid(), ProcStart: start}); err != nil {
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
	clearStateIfOwner()
	return nil
}

// maybeShutdown decides under the proxy lock whether the daemon may stop, and if so
// shuts the listener (releasing the port) and clears its own state — all WITHIN the
// lock so a concurrent EnsureDaemon never observes a half-torn-down daemon (state
// gone but the port still held) nor has its own fresh state deleted by us. It may
// stop only when no unexpired launch lease and no live codex worker remain and it
// has been idle past the grace period; any introspection uncertainty (-1 workers)
// keeps it alive.
func maybeShutdown(port int, srv *server, httpSrv *http.Server) bool {
	stopped := false
	_ = withProxyLock(func() error {
		if activeLeases() > 0 {
			return nil
		}
		if workers := liveCodexWorkers(port); workers != 0 {
			return nil // >0 live, or -1 unknown -> stay alive
		}
		if time.Since(time.Unix(0, srv.lastActivity.Load())) < idleGrace {
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = httpSrv.Shutdown(ctx)
		cancel()
		clearStateIfOwner()
		stopped = true
		return nil
	})
	return stopped
}

func clearState() {
	if p, err := statePath(); err == nil {
		os.Remove(p)
	}
}

// clearStateIfOwner removes the state file only when it still describes THIS
// process, so a shutting-down daemon never deletes a replacement's fresh state.
func clearStateIfOwner() {
	if st, err := readState(); err == nil && st.PID != os.Getpid() {
		return
	}
	clearState()
}

// StopDaemon stops a running daemon (explicit `cc-fleet codex-proxy stop` / uninstall).
func StopDaemon() error {
	return withProxyLock(func() error {
		st, err := readState()
		if err != nil {
			return nil // not running
		}
		if st.PID > 0 && pidAlive(st.PID, st.ProcStart) {
			if proc, e := os.FindProcess(st.PID); e == nil {
				_ = proc.Signal(os.Interrupt)
			}
		}
		clearState()
		return nil
	})
}

// Status reports whether a daemon is running and on which port.
func Status() (running bool, port int) {
	st, err := readState()
	if err != nil || st.PID <= 0 || !pidAlive(st.PID, st.ProcStart) {
		return false, 0
	}
	return true, st.Port
}

// Purge stops the daemon and removes every codex-proxy state file (state,
// handshake secret, leases, lock files) for uninstall. The login token chain
// is a credential: kept when keepToken (uninstall's KeepSecrets), else
// removed. Best-effort — failures land in kept with the reason, never abort.
func Purge(keepToken bool) (removed, kept []string) {
	_ = StopDaemon() // clears the state file

	paths := make([]string, 0, 4)
	for _, f := range []func() (string, error){secretPath, lockPath} {
		if p, err := f(); err == nil {
			paths = append(paths, p)
		}
	}
	if p, err := joinConfig(".cc-fleet-codex-token.lock"); err == nil {
		paths = append(paths, p)
	}
	tokenPath, terr := storePath()
	if terr == nil && !keepToken {
		paths = append(paths, tokenPath)
	}
	for _, p := range paths {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			kept = append(kept, fmt.Sprintf("%s (remove failed: %v)", p, err))
			continue
		}
		removed = append(removed, p)
	}
	if terr == nil && keepToken {
		if _, err := os.Stat(tokenPath); err == nil {
			kept = append(kept, tokenPath)
		}
	}
	if d, err := leasesDir(); err == nil {
		if err := os.RemoveAll(d); err != nil {
			kept = append(kept, fmt.Sprintf("%s (rm -rf failed: %v)", d, err))
		} else {
			removed = append(removed, d)
		}
	}
	return removed, kept
}
