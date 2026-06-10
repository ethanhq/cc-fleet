package codexproxy

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"

	"github.com/ethanhq/cc-fleet/internal/config"
)

// Loopback port selection for the codex provider's base_url: a small reserved
// scan range starting at a fixed literal. The chosen port is persisted into
// providers.toml at add time and baked into the cached profile, so it must stay
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

// ChoosePort picks a new daemon-backed provider's loopback port: the explicit
// preference when usable, else the first usable port in the reserved range.
// Usable = not already assigned to another provider in providers.toml AND (free to
// bind, or held by a live cc-fleet daemon — re-adding while the daemon runs must
// not fail). Skipping assigned ports keeps two daemon-backed providers from
// thrashing one port (daemons start lazily, so neither is bound at add time).
func ChoosePort(preferred int) (int, error) {
	assigned := assignedPorts()
	if preferred > 0 {
		if !assigned[preferred] && portUsable(preferred) {
			return preferred, nil
		}
		return 0, fmt.Errorf("port %d is in use or already assigned; pass a different --port", preferred)
	}
	for p := defaultPortBase; p < defaultPortBase+portScanWidth; p++ {
		if !assigned[p] && portUsable(p) {
			return p, nil
		}
	}
	return 0, fmt.Errorf("no free port in %d-%d; pass --port", defaultPortBase, defaultPortBase+portScanWidth-1)
}

// assignedPorts is the set of loopback ports already bound to a daemon-backed
// provider in providers.toml, so a new provider never reuses one. A config-load
// failure yields an empty set (the bind check below still guards live ports).
func assignedPorts() map[int]bool {
	used := map[int]bool{}
	cfg, err := config.Load()
	if err != nil {
		return used
	}
	for _, v := range cfg.Providers {
		if !v.DaemonBacked() {
			continue
		}
		if p, err := PortFromBaseURL(v.BaseURL); err == nil {
			used[p] = true
		}
	}
	return used
}

func portUsable(port int) bool {
	if !whoHoldsPort(port) {
		return true // free to bind
	}
	st, err := readState(port)
	return err == nil && st.Port == port && st.PID > 0 && pidAlive(st.PID, st.ProcStart) && portResponds(port)
}
