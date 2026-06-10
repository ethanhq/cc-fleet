// Package teamhist persists a board-observed snapshot of every live team so a
// team cleaned up by its lead keeps a presence on the Agents Board until
// the user deletes the record. The records are pure observability shadow data:
// no locks, last-write-wins between concurrent boards, AtomicWrite for crash
// safety. Live teams always render from live discovery — a record is consulted
// only for a team that has vanished from discovery.
//
// Records live at <ConfigDir>/teams-history/<team>.json (0600, dir 0700),
// alongside a <team>.del tombstone that a Delete leaves so a stale sibling
// board's last live observation can't resurrect a just-deleted record. The
// directory is distinct from subagent-jobs, so subagent-gc never enumerates it.
package teamhist

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/fileutil"
	"github.com/ethanhq/cc-fleet/internal/ids"
	"github.com/ethanhq/cc-fleet/internal/pinned"
	"github.com/ethanhq/cc-fleet/internal/teardown"
)

// historyDirName is the teams-history subdirectory under ConfigDir.
const historyDirName = "teams-history"

// tombstoneExt marks a deleted team: Delete writes <team>.del, and Upsert skips
// re-recording a team whose tombstone is newer than its newest live member's
// JoinedAt (a stale board observation can't resurrect a delete).
const tombstoneExt = ".del"

// rewriteInterval bounds write churn: Upsert rewrites a content-identical record
// only once its on-disk LastSeen has aged past this, so a fast refresh loop
// doesn't rewrite every record every tick.
const rewriteInterval = 60 * time.Second

// MemberRec is one recorded teammate's identity that survives config deletion:
// the snapshot the card needs to keep rendering transcripts after the team is
// gone. Cwd + LeadSessionID are per-member because a team can span lead sessions
// and working directories. PID is excluded — it is runtime-only.
type MemberRec struct {
	Name          string `json:"name"`
	Provider      string `json:"provider,omitempty"`
	Model         string `json:"model,omitempty"`
	SpawnTime     int64  `json:"spawn_time,omitempty"` // Member.JoinedAt, unix millis
	LeadSessionID string `json:"lead_session_id,omitempty"`
	Cwd           string `json:"cwd,omitempty"`
}

// Record is one team's persisted board snapshot. LastSeen is the last time the
// board observed this team live (RFC3339 UTC); it drives the card's
// "last seen <ts>" line and the write-churn guard.
type Record struct {
	Team     string      `json:"team"`
	LastSeen string      `json:"last_seen"`
	Members  []MemberRec `json:"members"`
}

// historyDir returns <ConfigDir>/teams-history.
func historyDir() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, historyDirName), nil
}

// recordPath returns teams-history/<team><ext>, validating team first (it
// becomes a path component) and confirming the result stays under the dir.
func recordPath(team, ext string) (string, error) {
	if err := ids.ValidateTeamName(team); err != nil {
		return "", err
	}
	dir, err := historyDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, team+ext)
	if err := ids.EnsureUnderRoot(dir, path); err != nil {
		return "", err
	}
	return path, nil
}

// newestJoinedAt returns the latest Member.JoinedAt across a team's live members
// (0 when none recorded a spawn time).
func newestJoinedAt(members []teardown.Teammate) int64 {
	var newest int64
	for _, t := range members {
		if t.SpawnTime > newest {
			newest = t.SpawnTime
		}
	}
	return newest
}

// Upsert records each live team's snapshot. It groups live teammates by team and
// writes one record per team, resolving each member's cwd via cwdOf(leadSessionID)
// (teammate discovery carries no per-member cwd; the lead session's recorded
// working directory is the snapshot the card replays). A team whose <team>.del
// tombstone is newer than its newest live member's JoinedAt is skipped — a stale board's last live
// observation can't resurrect a just-deleted record; a member whose JoinedAt is
// newer than the tombstone is a real respawn, so the tombstone is cleared and the
// team re-recorded. A content-identical record is rewritten only once its LastSeen
// has aged past rewriteInterval (the write-churn guard). Best-effort: a per-team
// error never aborts the rest, and an empty live set is a no-op.
func Upsert(live []teardown.Teammate, cwdOf func(sessionID string) string) error {
	byTeam := map[string][]teardown.Teammate{}
	order := []string{}
	for _, t := range live {
		if t.Team == "" {
			continue
		}
		if _, ok := byTeam[t.Team]; !ok {
			order = append(order, t.Team)
		}
		byTeam[t.Team] = append(byTeam[t.Team], t)
	}
	now := time.Now().UTC()
	for _, team := range order {
		members := byTeam[team]
		if tombstoneBlocks(team, newestJoinedAt(members)) {
			continue
		}
		// Clear a tombstone only on respawn evidence: one EXISTS but the members'
		// JoinedAt is newer (it no longer blocks). A Delete racing in after these
		// checks leaves its fresh tombstone in place — the record write below may
		// recreate the file, but List treats a record shadowed by a blocking
		// tombstone as deleted, so the race never resurrects anything.
		if tombstoneExists(team) {
			clearTombstone(team)
		}
		rec := Record{Team: team, LastSeen: now.Format(time.RFC3339)}
		for _, t := range members {
			var cwd string
			if cwdOf != nil {
				cwd = cwdOf(t.LeadSessionID)
			}
			rec.Members = append(rec.Members, MemberRec{
				Name:          t.Name,
				Provider:      t.Provider,
				Model:         t.Model,
				SpawnTime:     t.SpawnTime,
				LeadSessionID: t.LeadSessionID,
				Cwd:           cwd,
			})
		}
		writeIfChanged(team, rec, now)
	}
	return nil
}

// newestRecJoinedAt is the newest SpawnTime across a record's members (the same
// generation marker newestJoinedAt derives from live teammates).
func newestRecJoinedAt(rec Record) int64 {
	var newest int64
	for _, m := range rec.Members {
		if m.SpawnTime > newest {
			newest = m.SpawnTime
		}
	}
	return newest
}

// tombstoneBlocks reports whether team's tombstone is newer than newestJoinedAt
// (so Upsert must not re-record it). A newer member JoinedAt — a real respawn —
// does not block. A missing tombstone never blocks. JoinedAt is unix millis.
func tombstoneBlocks(team string, newestJoinedAt int64) bool {
	path, err := recordPath(team, tombstoneExt)
	if err != nil {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.ModTime().UnixMilli() >= newestJoinedAt
}

// tombstoneExists reports whether team has a tombstone file at all.
func tombstoneExists(team string) bool {
	path, err := recordPath(team, tombstoneExt)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

// clearTombstone removes team's tombstone (best-effort) — called when a real
// respawn re-records the team.
func clearTombstone(team string) {
	if path, err := recordPath(team, tombstoneExt); err == nil {
		_ = os.Remove(path)
	}
}

// writeIfChanged persists rec unless an identical record (ignoring LastSeen) is
// already on disk AND its LastSeen is younger than rewriteInterval — the
// write-churn guard. Best-effort: a write failure is swallowed (the next refresh
// retries). Creates the dir 0700, writes 0600.
func writeIfChanged(team string, rec Record, now time.Time) {
	path, err := recordPath(team, ".json")
	if err != nil {
		return
	}
	if prev, ok := readRecord(path); ok && sameContent(prev, rec) {
		if ts, perr := time.Parse(time.RFC3339, prev.LastSeen); perr == nil && now.Sub(ts) < rewriteInterval {
			return // unchanged and fresh — skip the rewrite
		}
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return
	}
	_ = fileutil.AtomicWrite(path, data, 0o600)
}

// sameContent reports whether two records carry the same team + members,
// ignoring LastSeen (the only field that changes every observation).
func sameContent(a, b Record) bool {
	if a.Team != b.Team || len(a.Members) != len(b.Members) {
		return false
	}
	for i := range a.Members {
		if a.Members[i] != b.Members[i] {
			return false
		}
	}
	return true
}

// readRecord parses one record file (ok=false on any read/parse error).
func readRecord(path string) (Record, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Record{}, false
	}
	var rec Record
	if json.Unmarshal(data, &rec) != nil {
		return Record{}, false
	}
	return rec, true
}

// List returns every team-history record. Team and member names are re-validated
// via ids on read (they feed transcript path joins downstream), and a record
// carrying an invalid name — or an unparseable file — is silently skipped. A
// missing dir → (nil, nil).
func List() ([]Record, error) {
	dir, err := historyDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("teamhist: read dir: %w", err)
	}
	var out []Record
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		rec, ok := readRecord(filepath.Join(dir, name))
		if !ok || ids.ValidateTeamName(rec.Team) != nil {
			continue
		}
		// A record shadowed by a blocking tombstone is deleted — a stale sibling
		// board's Upsert may have recreated the file right after a Delete.
		if tombstoneBlocks(rec.Team, newestRecJoinedAt(rec)) {
			continue
		}
		valid := true
		for _, m := range rec.Members {
			if ids.ValidateMemberName(m.Name) != nil {
				valid = false
				break
			}
		}
		if valid {
			out = append(out, rec)
		}
	}
	return out, nil
}

// Delete removes a team's record and writes a tombstone in its place, so a stale
// sibling board's last live observation can't immediately re-create the record
// (see tombstoneBlocks). The id is validated before it becomes any path component.
func Delete(team string) error {
	jsonPath, err := recordPath(team, ".json")
	if err != nil {
		return err
	}
	tomb, err := recordPath(team, tombstoneExt)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(tomb), 0o700); err != nil {
		return err
	}
	if err := fileutil.AtomicWrite(tomb, nil, 0o600); err != nil {
		return err
	}
	if err := os.Remove(jsonPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	// The record is gone — drop any keep-marker so a same-name respawn starts unpinned.
	return pinned.Unpin(pinned.Team, team)
}

// ClearEnded deletes the ended-team records owned by sessionID — every record whose members
// ALL carry LeadSessionID == sessionID — skipping any pinned team. It is the teamhist half of
// the board "clear-finished" action (session-scoped). A record with a member from another
// session is left intact: Delete removes a whole-team record, so clearing one session must not
// take another session's members with it (the user removes such a mixed record explicitly).
// Pins are honored from the caller's snapshot. Returns the number of records deleted.
func ClearEnded(sessionID string, pins pinned.Set) (int, error) {
	if sessionID == "" {
		return 0, fmt.Errorf("teamhist: ClearEnded requires a session id")
	}
	recs, err := List()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, rec := range recs {
		if pins.Has(pinned.Team, rec.Team) || !whollyInSession(rec, sessionID) {
			continue
		}
		if derr := Delete(rec.Team); derr == nil {
			deleted++
		}
	}
	return deleted, nil
}

// whollyInSession reports whether rec has at least one member and every member's LeadSessionID
// is sessionID — i.e. the team is owned solely by that session.
func whollyInSession(rec Record, sessionID string) bool {
	if len(rec.Members) == 0 {
		return false
	}
	for _, m := range rec.Members {
		if m.LeadSessionID != sessionID {
			return false
		}
	}
	return true
}

// Purge removes the whole teams-history directory (cc-fleet uninstall). Returns
// the dir path so the caller can report it. A missing dir is not an error.
func Purge() (string, error) {
	dir, err := historyDir()
	if err != nil {
		return "", err
	}
	if err := os.RemoveAll(dir); err != nil {
		return dir, err
	}
	return dir, nil
}
