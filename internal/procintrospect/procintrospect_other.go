//go:build !linux && !darwin && !windows

package procintrospect

import "errors"

// ErrUnsupported is returned by the table/argv readers on platforms with
// neither a Linux /proc nor the ps/pgrep guarantees darwin relies on. cc-fleet
// still builds and runs, it just can't introspect processes; the main
// spawn/teardown paths, which are pane-centric (tmux), are unaffected.
var ErrUnsupported = errors.New("procintrospect: process introspection unsupported on this platform")

func Cmdline(pid int) ([]string, error) { return nil, ErrUnsupported }
func Children(pid int) []int            { return nil }
func ProcessTable() ([]Process, error)  { return nil, ErrUnsupported }
func Ppid(pid int) (int, bool)          { return 0, false }
func ProcStart(pid int) (string, bool)  { return "", false }
