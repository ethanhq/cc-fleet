package codexproxy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Loopback port selection for the codex vendor's base_url: a small reserved
// scan range starting at a fixed literal. The chosen port is persisted into
// vendors.toml at add time and baked into the cached profile, so it must stay
// stable across daemon restarts (never ephemeral).
const (
	defaultPortBase = 17222
	portScanWidth   = 10
)

// ScanDefaultModel reads the default model from the codex CLI's
// ~/.codex/config.toml — the one sanctioned read of that tree (never auth).
// Falls back when the file or key is absent.
func ScanDefaultModel(fallback string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return fallback
	}
	var doc struct {
		Model string `toml:"model"`
	}
	if _, err := toml.DecodeFile(filepath.Join(home, ".codex", "config.toml"), &doc); err != nil || doc.Model == "" {
		return fallback
	}
	return doc.Model
}

// ChoosePort picks the daemon's loopback port for a new codex vendor: the
// explicit preference when usable, else the first usable port in the reserved
// range. Usable = free to bind, or already held by a live cc-fleet codex
// daemon (re-adding the vendor while the daemon runs must not fail).
func ChoosePort(preferred int) (int, error) {
	if preferred > 0 {
		if portUsable(preferred) {
			return preferred, nil
		}
		return 0, fmt.Errorf("port %d is held by another process; free it or pass a different --port", preferred)
	}
	for p := defaultPortBase; p < defaultPortBase+portScanWidth; p++ {
		if portUsable(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d-%d; pass --port", defaultPortBase, defaultPortBase+portScanWidth-1)
}

func portUsable(port int) bool {
	if !whoHoldsPort(port) {
		return true // free to bind
	}
	st, err := readState()
	return err == nil && st.Port == port && st.PID > 0 && pidAlive(st.PID, st.ProcStart) && portResponds(port)
}
