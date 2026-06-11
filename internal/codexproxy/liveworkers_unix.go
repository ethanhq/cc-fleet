//go:build !windows

package codexproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// liveWorkers counts running claude processes whose --settings profile points at
// this proxy's port. A scan error returns -1 ("unknown" -> daemon stays alive).
func liveWorkers(port int) int {
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
