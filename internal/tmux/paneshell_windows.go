//go:build windows

package tmux

import (
	"os"
	"os/exec"
	"strings"
)

// windowsTmuxOptInEnv mirrors cmd/cc-fleet's internal_helpers.go constant of the
// same name (that package can't be imported here — cmd depends on internal, not
// the reverse). Keep the two literals in sync.
const windowsTmuxOptInEnv = "CC_FLEET_WINDOWS_TMUX_EXPERIMENTAL"

// bashCandidates are the usual Git-for-Windows bash locations, tried in order
// when CC_FLEET_WINDOWS_BASH is unset.
var bashCandidates = []string{
	`C:\Program Files\Git\usr\bin\bash.exe`,
	`C:\Program Files\Git\bin\bash.exe`,
	`C:\Program Files (x86)\Git\usr\bin\bash.exe`,
}

// windowsBashPath resolves the POSIX shell used to run pane commands. An empty
// result means no bash was found; wrapPaneCommand then leaves the command alone
// so the caller still sees the raw shell failure rather than a silent no-op.
func windowsBashPath() string {
	if p := os.Getenv("CC_FLEET_WINDOWS_BASH"); p != "" {
		return p
	}
	for _, c := range bashCandidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	if p, err := exec.LookPath("bash.exe"); err == nil {
		return p
	}
	return ""
}

// wrapPaneCommand re-expresses a POSIX pane command so the Windows tmux port
// (psmux) can run it. psmux ignores default-shell for shell-commands and always
// runs `powershell.exe -NoLogo -Command "<cmd>"`, so a POSIX line dies on the
// first `env -u`. We hand the line to Git bash via PowerShell's call operator.
//
// Two constraints follow from that wrapper: no double quote may appear in the
// result (psmux already wraps the whole string in one), and a literal single
// quote inside a PowerShell '…' string is written as two. PATH is seeded because
// a non-login `bash -c` does not see /usr/bin, so `env` would not resolve.
//
// Gated on windowsTmuxOptInEnv, same as onWindows: off by default so `go test`
// exercises the tmux package exactly as it does upstream (against a faked tmux
// binary on PATH, expecting the raw cmd argv unwrapped), and a plain `GOOS=windows
// go build` without the opt-in never emits a PowerShell-specific command line.
func wrapPaneCommand(cmd string) string {
	if os.Getenv(windowsTmuxOptInEnv) == "" {
		return cmd
	}
	bash := windowsBashPath()
	if bash == "" || cmd == "" {
		return cmd
	}
	inner := "export PATH=/usr/bin:/bin:$PATH; " + cmd
	return "& '" + strings.ReplaceAll(bash, "'", "''") +
		"' -c '" + strings.ReplaceAll(inner, "'", "''") + "'"
}
