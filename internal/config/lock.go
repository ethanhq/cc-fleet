package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/ids"
)

// teamLockBasename is the per-team lock file inside ~/.claude/teams/<team>/.
//
// Dotfile name is intentional: tmux / users browsing the team dir don't see
// it by default, and CC's own tasks/.lock lives elsewhere — we never want
// to share a lock with the upstream binary's state files.
const teamLockBasename = ".cc-fleet-lock"

// serverLockBasename is a single process-wide lock at $HOME/.claude/ that
// serializes tmux operations racing at the SERVER level (split-window +
// select-layout main-vertical), which per-team locks cannot serialize across
// different teams spawning into the same tmux window. See WithServerLock.
const serverLockBasename = ".cc-fleet-tmux.lock"

// vendorsLockBasename is a single process-wide lock co-located with the global
// vendors.toml (inside ConfigDir). It serializes the load→mutate→save cycle of
// `cc-fleet add` / `edit` / `remove`, which all rewrite that one global file:
// without it two concurrent CLI mutations each read the same old config and the
// later Save clobbers the earlier writer's update (lost update).
// Co-locating the lock with vendors.toml keeps it inside that file's ownership
// boundary (ConfigDir, not ~/.claude/).
const vendorsLockBasename = ".cc-fleet-vendors.lock"

// WithTeamLock acquires an exclusive flock on
// $HOME/.claude/teams/<team>/.cc-fleet-lock, runs fn, then releases the lock.
//
// The lock is held for the full duration of fn (blocking flock, LOCK_EX). The
// parent directory is created at 0700 and the lock file at 0600 if missing.
//
// cc-fleet is an external process, so it cannot rely on in-process
// serialization. Every cc-fleet code path that mutates per-team state
// (config.json members list, inbox files, profile install) must run under this
// lock. The kernel guarantees mutual exclusion across processes via flock on
// the same file descriptor inode.
//
// team must be non-empty; an empty team name is a programmer error.
func WithTeamLock(team string, fn func() error) error {
	if team == "" {
		return errors.New("config: WithTeamLock: empty team name")
	}
	if fn == nil {
		return errors.New("config: WithTeamLock: nil fn")
	}

	path, err := teamLockPath(team)
	if err != nil {
		return err
	}
	return withFlock(path, fn)
}

// WithServerLock acquires a single process-wide exclusive flock at
// $HOME/.claude/.cc-fleet-tmux.lock, runs fn, then releases it.
//
// It serializes operations that race at the tmux-SERVER (window-layout) level —
// chiefly split-window + select-layout main-vertical + resize-pane — which
// mutate state NOT scoped to any one team. WithTeamLock does not serialize
// spawns from DIFFERENT teams into the same tmux window (their per-team locks
// sit on different inodes), so the tmux split sequence must additionally hold
// this global lock. cc-fleet only ever uses the default tmux server, so one
// global lock is effectively per-server.
//
// Lock ordering: callers already holding a team lock acquire this one INSIDE it
// (team outer, server inner). The server lock is a single global resource, so
// there is no lock-ordering cycle and no deadlock.
func WithServerLock(fn func() error) error {
	if fn == nil {
		return errors.New("config: WithServerLock: nil fn")
	}
	path, err := serverLockPath()
	if err != nil {
		return err
	}
	return withFlock(path, fn)
}

// WithVendorsConfigLock acquires a single process-wide exclusive flock at
// <ConfigDir>/.cc-fleet-vendors.lock, runs fn, then releases it.
//
// It guards the GLOBAL vendors.toml lifecycle (add / edit / remove): the full
// config.Load → mutate → config.Save cycle must run under it so concurrent CLI
// mutations serialize instead of clobbering each other.
//
// Lock ordering: this is a THIRD, independent scope alongside WithTeamLock
// (per-team config.json) and WithServerLock (tmux window race). The three guard
// disjoint resources (global vendors.toml vs a per-team dir vs the tmux server),
// so no acquisition cycle exists today. If a future flow ever needs more than
// one at once, acquire this vendors-config lock OUTERMOST — it covers a global
// file touched before any team/tmux work — then team, then server inner.
func WithVendorsConfigLock(fn func() error) error {
	if fn == nil {
		return errors.New("config: WithVendorsConfigLock: nil fn")
	}
	path, err := vendorsLockPath()
	if err != nil {
		return err
	}
	return withFlock(path, fn)
}

// withFlock opens path (creating parents at 0700 and the file at 0600), takes a
// blocking exclusive flock, runs fn, then releases. We deliberately do NOT use
// LOCK_NB — concurrent holders should serialize behind each other, not error
// out. The kernel guarantees mutual exclusion across processes via flock
// on the same inode.
func withFlock(path string, fn func() error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: mkdir %s: %w", dir, err)
	}
	// O_CREATE|O_RDWR: we only need the inode for flock, but RDWR lets
	// flock(LOCK_EX) succeed on all kernels regardless of mount options.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("config: open lock %s: %w", path, err)
	}
	defer f.Close()
	if err := lockFile(f); err != nil {
		return fmt.Errorf("config: flock %s: %w", path, err)
	}
	defer func() {
		// Best-effort unlock; the kernel also releases on Close.
		unlockFile(f)
	}()
	return fn()
}

// teamLockPath returns $HOME/.claude/teams/<team>/.cc-fleet-lock.
//
// $HOME is required — XDG does not apply here because Claude Code reads
// ~/.claude/ unconditionally.
//
// Defense-in-depth: team is path-validated before joining; the
// constructed lock path is under-root checked against $HOME/.claude/teams so
// a hostile name can never plant a lock file outside cc-fleet's ownership
// boundary. CLI entry points already validate; this is belt-and-braces.
func teamLockPath(team string) (string, error) {
	if err := ids.ValidateTeamName(team); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("config: HOME is not set")
	}
	root := filepath.Join(home, ".claude", "teams")
	out := filepath.Join(root, team, teamLockBasename)
	if err := ids.EnsureUnderRoot(root, out); err != nil {
		return "", fmt.Errorf("config: %w", err)
	}
	return out, nil
}

// serverLockPath returns $HOME/.claude/.cc-fleet-tmux.lock — the single global
// lock shared by every cc-fleet process (see WithServerLock).
func serverLockPath() (string, error) {
	home := os.Getenv("HOME")
	if home == "" {
		return "", errors.New("config: HOME is not set")
	}
	return filepath.Join(home, ".claude", serverLockBasename), nil
}

// vendorsLockPath returns <ConfigDir>/.cc-fleet-vendors.lock — the single global
// lock guarding the vendors.toml load→mutate→save cycle (see
// WithVendorsConfigLock). It lives in ConfigDir so it shares vendors.toml's
// ownership boundary and honors $XDG_CONFIG_HOME.
func vendorsLockPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, vendorsLockBasename), nil
}
