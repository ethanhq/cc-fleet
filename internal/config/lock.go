package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// serverLockBasename is a single process-wide lock at $HOME/.claude/ that
// serializes tmux operations racing at the SERVER level (break-pane / join-pane
// + select-layout main-vertical). See WithServerLock.
const serverLockBasename = ".cc-fleet-tmux.lock"

// providersLockBasename is a single process-wide lock co-located with the global
// providers.toml (inside ConfigDir). It serializes the load→mutate→save cycle of
// `cc-fleet add` / `edit` / `remove`, which all rewrite that one global file:
// without it two concurrent CLI mutations each read the same old config and the
// later Save clobbers the earlier writer's update (lost update).
// Co-locating the lock with providers.toml keeps it inside that file's ownership
// boundary (ConfigDir, not ~/.claude/).
const providersLockBasename = ".cc-fleet-providers.lock"

// WithServerLock acquires a single process-wide exclusive flock at
// $HOME/.claude/.cc-fleet-tmux.lock, runs fn, then releases it.
//
// It serializes operations that race at the tmux-SERVER (window-layout) level —
// chiefly hide/show's break-pane / join-pane + select-layout main-vertical +
// resize-pane — which mutate state NOT scoped to any one team. One global lock
// covers every tmux server.
//
// Lock ordering: a caller that also holds the providers-config lock acquires
// this one INSIDE it (providers outer, server inner). The server lock is a
// single global resource, so there is no lock-ordering cycle and no deadlock.
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

// WithProvidersConfigLock acquires a single process-wide exclusive flock at
// <ConfigDir>/.cc-fleet-providers.lock, runs fn, then releases it.
//
// It guards the GLOBAL providers.toml lifecycle (add / edit / remove): the full
// config.Load → mutate → config.Save cycle must run under it so concurrent CLI
// mutations serialize instead of clobbering each other.
//
// Lock ordering: this is an independent scope alongside WithServerLock (tmux
// window race). The two guard disjoint resources (global providers.toml vs the
// tmux server), so no acquisition cycle exists today. If a future flow ever
// needs both, acquire this providers-config lock OUTERMOST — it covers a global
// file touched before any tmux work — then server inner.
func WithProvidersConfigLock(fn func() error) error {
	if fn == nil {
		return errors.New("config: WithProvidersConfigLock: nil fn")
	}
	path, err := providersLockPath()
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

// WithFlock is the exported generic blocking-exclusive flock primitive (the same
// withFlock the two config scopes use), for any cross-process critical section
// keyed by a file path. The CALLER owns path validation and choosing a safe lock
// path (it is created lazily and the kernel locks its inode). See
// subagent.WithRunLock for the workflow runtime's per-run execution lock.
func WithFlock(path string, fn func() error) error {
	return withFlock(path, fn)
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

// providersLockPath returns <ConfigDir>/.cc-fleet-providers.lock — the single global
// lock guarding the providers.toml load→mutate→save cycle (see
// WithProvidersConfigLock). It lives in ConfigDir so it shares providers.toml's
// ownership boundary and honors $XDG_CONFIG_HOME.
func providersLockPath() (string, error) {
	dir, err := ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, providersLockBasename), nil
}
