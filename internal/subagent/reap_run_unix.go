//go:build !windows

package subagent

import (
	"syscall"
	"time"

	"github.com/ethanhq/cc-fleet/internal/procintrospect"
)

// reapEngineTree reaps a workflow engine's whole process tree by ANCESTRY — the engine
// process plus its in-flight provider-leaf `claude` children and their grandchildren —
// not merely the engine's own process group. This matters because each leaf claude makes
// itself its OWN group leader (runClaude → setGroupAttr → Setpgid), so a bare
// kill(-EnginePID) reaches only the engine's group and leaves the leaves running; they
// would keep burning provider quota until each hit its per-agent timeout. We walk the
// descendant set (procintrospect.Children, linux /proc + darwin ps) and signal each pid
// AND its group (the negative-pid catches a leader's grandchildren that inherited its
// pgid): SIGTERM, a short grace, then SIGKILL to survivors. Mirrors the Windows
// taskkill /T ancestry semantics. Best-effort — an already-gone pid/group is fine.
func reapEngineTree(pid int) {
	if pid <= 0 {
		return
	}
	pids := descendantTree(pid)
	for _, p := range pids {
		_ = syscall.Kill(p, syscall.SIGTERM)
		_ = syscall.Kill(-p, syscall.SIGTERM)
	}
	time.Sleep(200 * time.Millisecond)
	for _, p := range pids {
		if pidAlive(p) {
			_ = syscall.Kill(p, syscall.SIGKILL)
		}
		// Probe the GROUP separately: a leader can die in the grace while an unenumerated group
		// member (re-parented, or missed by a degraded Children) survives — it still answers on -p.
		if err := syscall.Kill(-p, 0); err == nil {
			_ = syscall.Kill(-p, syscall.SIGKILL)
		}
	}
}

// descendantTree returns root plus all of its transitive descendants (BFS via
// procintrospect.Children). On a platform without process introspection Children returns
// nil, so it degrades to just [root] (the engine group is still reaped by reapEngineTree's
// negative-pid signal; only the separately-grouped leaves can't be found there).
func descendantTree(root int) []int {
	out := []int{root}
	seen := map[int]bool{root: true}
	for queue := []int{root}; len(queue) > 0; {
		p := queue[0]
		queue = queue[1:]
		for _, c := range procintrospect.Children(p) {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
				queue = append(queue, c)
			}
		}
	}
	return out
}
