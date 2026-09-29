//go:build darwin

package procintrospect

import (
	"encoding/binary"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

const procHelperEnv = "CCF_PROCINTROSPECT_HELPER_SLEEP"

// TestHelperProcess is not a real test. When procHelperEnv is set it blocks so
// a parent test can read its argv, and is killed by that test's cleanup.
func TestHelperProcess(t *testing.T) {
	if os.Getenv(procHelperEnv) != "1" {
		return
	}
	time.Sleep(time.Minute)
	os.Exit(0)
}

// TestCmdlineExactArgvWithSpaces starts a real child whose argv carries spaces,
// quotes and an empty string, and asserts Cmdline returns it element by element.
func TestCmdlineExactArgvWithSpaces(t *testing.T) {
	want := []string{
		os.Args[0], "-test.run=^TestHelperProcess$", "--", // "--" ends flag parsing
		"--settings", "/Users/jo bloggs/.claude/profiles/deep seek.json",
		`it's "quoted"`, "", "tab\there", "",
	}
	cmd := exec.Command(want[0], want[1:]...)
	cmd.Env = append(os.Environ(), procHelperEnv+"=1")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	var got []string
	var err error
	for i := 0; i < 40; i++ { // the child may not have exec'd yet
		got, err = Cmdline(cmd.Process.Pid)
		if err == nil && reflect.DeepEqual(got, want) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("Cmdline(%d) = %q, %v; want %q", cmd.Process.Pid, got, err, want)
}

// TestProcessTableContainsSelf asserts the table scan returns this test process
// with its exact argv.
func TestProcessTableContainsSelf(t *testing.T) {
	procs, err := ProcessTable()
	if err != nil {
		t.Fatalf("ProcessTable: %v", err)
	}
	for _, p := range procs {
		if p.PID == os.Getpid() {
			if !reflect.DeepEqual(p.Argv, os.Args) {
				t.Fatalf("ProcessTable argv for self = %q, want %q", p.Argv, os.Args)
			}
			return
		}
	}
	t.Fatalf("ProcessTable (%d rows) does not contain self pid %d", len(procs), os.Getpid())
}

func procArgsBuf(argc int32, body string) []byte {
	b := make([]byte, 4, 4+len(body))
	binary.LittleEndian.PutUint32(b, uint32(argc))
	return append(b, body...)
}

// TestParseProcargs2 covers the layout edge cases a live process rarely shows.
func TestParseProcargs2(t *testing.T) {
	got, err := parseProcargs2(procArgsBuf(3, "/bin/x\x00\x00\x00\x00x\x00a b\x00\x00HOME=/h\x00"))
	if err != nil || !reflect.DeepEqual(got, []string{"x", "a b", ""}) {
		t.Fatalf("parse = %q, %v; want [x \"a b\" \"\"] (env ignored)", got, err)
	}
	for name, buf := range map[string][]byte{
		"short":         {1, 0},
		"no exec path":  procArgsBuf(1, "/bin/x"),
		"truncated":     procArgsBuf(2, "/bin/x\x00x\x00y"),
		"negative argc": procArgsBuf(-1, "/bin/x\x00"),
	} {
		if got, err := parseProcargs2(buf); err == nil {
			t.Errorf("%s: parse = %q, want error", name, got)
		}
	}
}
