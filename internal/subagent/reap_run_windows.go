//go:build windows

package subagent

// reapEngineTree reaps a workflow engine's whole process tree. On Windows killProcessTree
// uses `taskkill /T`, which is ALREADY ancestry-based (it terminates the target and every
// descendant), so the engine's in-flight provider-leaf children are reaped without a
// separate process-group walk.
func reapEngineTree(pid int) { killProcessTree(pid) }
