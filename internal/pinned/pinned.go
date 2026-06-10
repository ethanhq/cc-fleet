// Package pinned is an out-of-band registry of the board records a user has chosen
// to keep. A pin is a zero-byte marker file at ConfigDir/pinned/<kind>/<id>; it
// carries no content. The registry is deliberately separate from the record stores
// (subagent-jobs, teams-history) so the record writers — a board refresh's
// teamhist.Upsert, the engine's manifest SaveRun, a job's writeMeta — never touch a
// pin and so can never clobber it. GC and the board consult the registry to skip /
// mark pinned records; an explicit per-record delete (board `d`) Unpins, and
// uninstall Purges the whole dir.
package pinned

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
)

// pinnedDirName is the registry root under ConfigDir, holding one subdir per Kind.
const pinnedDirName = "pinned"

// Kind namespaces a pin by record type so a job, a run, and a team can share an id
// without colliding (each lives under its own subdir).
type Kind string

const (
	Job  Kind = "job"
	Run  Kind = "run"
	Team Kind = "team"
)

func validKind(k Kind) bool { return k == Job || k == Run || k == Team }

// validID checks id against the SAME canonical rules the record's own id uses, so anything that can
// exist on the board can be pinned (and no marker can escape its kind dir): team names via
// ids.ValidateTeamName, job/run ids via ids.ValidateJobID — identical path-component safety, just a
// different sentinel.
func validID(kind Kind, id string) error {
	if kind == Team {
		return ids.ValidateTeamName(id)
	}
	return ids.ValidateJobID(id)
}

func kindDir(k Kind) (string, error) {
	base, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, pinnedDirName, string(k)), nil
}

func markerPath(k Kind, id string) (string, error) {
	if !validKind(k) {
		return "", fmt.Errorf("pinned: invalid kind %q", k)
	}
	if err := validID(k, id); err != nil {
		return "", fmt.Errorf("pinned: invalid id: %w", err)
	}
	dir, err := kindDir(k)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id), nil
}

// Pin marks (kind,id) as user-kept. Idempotent (re-pinning rewrites the empty marker).
func Pin(kind Kind, id string) error {
	path, err := markerPath(kind, id)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.AtomicWrite(path, nil, 0o600)
}

// Unpin clears (kind,id)'s mark. A missing marker is not an error.
func Unpin(kind Kind, id string) error {
	path, err := markerPath(kind, id)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// Set is an in-memory snapshot of the registry — a map lookup so a GC sweep or a board
// refresh can test many records without one stat per record. The zero Set has no pins
// (Has always false), so a caller that fails to snapshot degrades safely to "nothing
// pinned" only where that is the intended fallback.
type Set struct {
	m map[Kind]map[string]bool
}

// Has reports whether (kind,id) was pinned when the snapshot was taken.
func (s Set) Has(kind Kind, id string) bool {
	return s.m[kind][id]
}

// With returns a copy of the set with (kind,id) pinned (add=true) or unpinned (add=false). The
// board applies it optimistically when it dispatches a pin toggle, so two quick presses before the
// registry write + reload land still alternate instead of both pinning off the same stale snapshot.
func (s Set) With(kind Kind, id string, add bool) Set {
	out := Set{m: map[Kind]map[string]bool{}}
	for k, ids := range s.m {
		cp := make(map[string]bool, len(ids))
		for i := range ids {
			cp[i] = true
		}
		out.m[k] = cp
	}
	if out.m[kind] == nil {
		out.m[kind] = map[string]bool{}
	}
	if add {
		out.m[kind][id] = true
	} else {
		delete(out.m[kind], id)
	}
	return out
}

// Snapshot reads the whole registry into a Set. A missing registry (or a missing kind
// subdir) yields an empty set for that kind — everything reads as unpinned.
func Snapshot() (Set, error) {
	s := Set{m: map[Kind]map[string]bool{}}
	for _, k := range []Kind{Job, Run, Team} {
		ids := map[string]bool{}
		s.m[k] = ids
		dir, err := kindDir(k)
		if err != nil {
			return s, err
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return s, fmt.Errorf("pinned: read %s dir: %w", k, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				ids[e.Name()] = true
			}
		}
	}
	return s, nil
}

// Purge removes the whole registry dir (cc-fleet uninstall). Returns the dir path so the
// caller can report it; a missing dir is not an error.
func Purge() (string, error) {
	base, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(base, pinnedDirName)
	if err := os.RemoveAll(dir); err != nil {
		return dir, err
	}
	return dir, nil
}
