//go:build !windows

package subagent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ethanhq/cc-fleet/internal/childenv"
)

// A provider worker runs with childenv.WorkerEnv, sync or background, in place of
// a value the launching shell carries for one of those names.
func TestRun_WorkerEnv(t *testing.T) {
	for _, bg := range []bool{false, true} {
		t.Run(fmt.Sprintf("background=%v", bg), func(t *testing.T) {
			_, envLog := nativeFake(t)
			xdg := t.TempDir()
			t.Setenv("XDG_CONFIG_HOME", xdg)
			t.Setenv("HOME", t.TempDir())
			writeMinimalProviders(t, xdg)
			t.Setenv("CLAUDE_CODE_DISABLE_WORKFLOWS", "0")

			res := Run(context.Background(), Request{Provider: "glm", Prompt: "hi", JSON: true, Background: bg})
			if !res.OK {
				t.Fatalf("run failed: %+v", res)
			}
			var env string
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				data, _ := os.ReadFile(envLog)
				if env = string(data); strings.Contains(env, "CLAUDE_CODE_DISABLE_WORKFLOWS=") {
					break
				}
			}
			for _, kv := range childenv.WorkerEnv {
				name := kv[:strings.IndexByte(kv, '=')+1]
				var seen []string
				for _, l := range strings.Split(env, "\n") {
					if strings.HasPrefix(l, name) {
						seen = append(seen, l)
					}
				}
				if len(seen) != 1 || seen[0] != kv {
					t.Errorf("child env has %q for %s, want just %q", seen, name, kv)
				}
			}
		})
	}
}
