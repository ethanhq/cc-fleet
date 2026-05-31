package subagent

import (
	"os"
	"strings"
	"testing"
	"time"
)

// spaceSettings is a per-vendor profile path under a HOME that contains a space.
// claudeBinSpace is the version-pinned claude binary under the same
// space-bearing HOME.
const (
	spaceSettings   = "/root/with space/.config/cc-fleet/profiles/glm.settings.json"
	claudeBinSpace  = "/root/with space/.local/share/claude/versions/2.1.150"
	otherSpaceSetts = "/root/with space/.config/cc-fleet/profiles/kimi.settings.json"
)

// darwinSplit models procintrospect.Cmdline's darwin behavior: `ps -o command=`
// space-joins argv and strings.Fields re-splits it, so a path with a space is
// torn across tokens (the lossy split that broke the exact --settings match).
func darwinSplit(cmdline string) []string { return strings.Fields(cmdline) }

// TestArgvIsClaudeJob_DarwinSplitSettingsWithSpace: a live claude job whose
// --settings path contains a space is recovered from darwin's lossy-split argv
// via the joined-argv substring fallback, while a DIFFERENT profile path must
// still NOT match (no false positive).
func TestArgvIsClaudeJob_DarwinSplitSettingsWithSpace(t *testing.T) {
	argv := darwinSplit(claudeBinSpace + " --settings " + spaceSettings + " --model glm-4.6 -p")

	// Precondition: the exact settings token is absent (it was split), which is
	// what broke the old exact-token match.
	for _, a := range argv {
		if a == spaceSettings {
			t.Fatal("precondition broken: darwin split should tear the space-bearing settings path")
		}
	}

	if !argvIsClaudeJob(argv, spaceSettings) {
		t.Fatal("a --settings path with a space must still match via the joined-argv fallback")
	}
	if argvIsClaudeJob(argv, otherSpaceSetts) {
		t.Fatal("a different --settings path must not match (no false positive from the fallback)")
	}
	// A non-claude process with the same settings substring still needs the
	// claude token, so it must NOT match.
	if argvIsClaudeJob(darwinSplit("/usr/bin/grep "+spaceSettings), spaceSettings) {
		t.Fatal("a non-claude process must not match even if it mentions the settings path")
	}
}

// TestStatusForAndGC_DarwinSpaceSettings_LiveJobStaysRunning: a live background
// job whose --settings path has a space must keep reading as running
// (StatusFor) and must NOT be GC'd. The reuse-guard argv reader is stubbed to
// the darwin lossy split so the matcher is exercised regardless of host; the
// PID is this live test process.
func TestStatusForAndGC_DarwinSpaceSettings_LiveJobStaysRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	dir, err := jobsDir()
	if err != nil {
		t.Fatalf("jobsDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir jobs dir: %v", err)
	}

	const jobID = "live-space-job"
	meta := jobMeta{
		JobID:        jobID,
		PID:          os.Getpid(), // a definitely-live pid
		PGID:         os.Getpid(),
		Vendor:       "glm",
		Model:        "glm-4.6",
		StartedAt:    time.Now().UTC().Format(time.RFC3339),
		Status:       "running",
		SettingsPath: spaceSettings,
	}
	if err := writeMeta(dir, meta); err != nil {
		t.Fatalf("writeMeta: %v", err)
	}

	origSeam := reuseGuardArgv
	reuseGuardArgv = func(int) ([]string, bool) {
		return darwinSplit(claudeBinSpace + " --settings " + spaceSettings + " -p"), true
	}
	t.Cleanup(func() { reuseGuardArgv = origSeam })

	if res := StatusFor(jobID); res.Status != "running" {
		t.Fatalf("StatusFor = %q, want running (live space-settings job mis-read as dead)", res.Status)
	}

	// GC with no age limit must KEEP a live job's files.
	if r := GC(0); !r.OK {
		t.Fatalf("GC: %+v", r)
	}
	if _, err := os.Stat(metaPath(dir, jobID)); err != nil {
		t.Fatalf("GC removed a LIVE job's meta (space-settings mis-read as dead): %v", err)
	}
}
