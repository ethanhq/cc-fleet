package codexproxy

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// shutdownClient bounds the /shutdown POST so a wedged daemon falls through to the
// platform fallback rather than blocking a stop.
var shutdownClient = &http.Client{Timeout: 2 * time.Second}

func shutdownURL(port int) string {
	return fmt.Sprintf("http://127.0.0.1:%d/shutdown", port)
}

// loadSecretOnly reads the handshake secret WITHOUT creating it (no secret lock),
// returning "" when the file is absent or empty. The stop paths read it before
// taking the per-port proxy lock — preserving the secret→proxy order EnsureDaemon
// uses (create the secret, then take the proxy lock) so a stopper cannot deadlock a
// concurrent ensure. A "" result skips the authenticated /shutdown and goes
// straight to the platform fallback.
func loadSecretOnly() string {
	p, err := secretPath()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// stopViaShutdown stops a live daemon. Primary (all platforms): an authenticated
// POST /shutdown on the loopback listener with a short client timeout, which drains
// in-flight conversions before closing. Fallback for a wedged daemon or a missing
// secret: unix sends os.Interrupt; windows terminates the process. The caller holds
// the port's proxy lock and clears state afterward.
func stopViaShutdown(port int, st proxyState, secret string) {
	if secret != "" && postShutdown(port, secret) {
		return
	}
	// Re-verify identity at the last instant: the daemon may have exited during
	// the POST window and its pid been recycled — never signal a stranger.
	if !pidAlive(st.PID, st.ProcStart) {
		return
	}
	stopProcess(st.PID)
}

// postShutdown sends the authenticated shutdown request and reports whether the
// daemon accepted it (200). A non-200 or a transport error falls through to the
// platform fallback.
func postShutdown(port int, secret string) bool {
	req, err := http.NewRequest(http.MethodPost, shutdownURL(port), nil)
	if err != nil {
		return false
	}
	req.Header.Set("x-api-key", secret)
	resp, err := shutdownClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
