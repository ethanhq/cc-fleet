package onboarding

import (
	"os"
	"runtime"
	"strings"
)

// TmuxInstallHint returns the OS-appropriate command to install tmux. It NEVER
// runs anything and NEVER uses sudo on the user's behalf: the string is shown
// for the user to run themselves. Detection is deliberately coarse —
// GOOS plus a light /etc/os-release sniff on Linux is enough to pick the right
// package manager; we don't enumerate every distro.
func TmuxInstallHint() string {
	switch runtime.GOOS {
	case "darwin":
		return "brew install tmux"
	case "linux":
		return linuxTmuxHint()
	default:
		return "install tmux with your platform's package manager"
	}
}

func linuxTmuxHint() string {
	return tmuxCmdForFamily(linuxFamily())
}

// tmuxCmdForFamily maps a coarse distro family to its install command. Split
// from linuxTmuxHint (which supplies the live family) so the mapping is
// unit-testable without a real /etc/os-release.
func tmuxCmdForFamily(family string) string {
	switch family {
	case "debian":
		return "sudo apt-get install tmux"
	case "fedora":
		return "sudo dnf install tmux"
	case "arch":
		return "sudo pacman -S tmux"
	case "alpine":
		return "sudo apk add tmux"
	default:
		return "install tmux with your package manager, e.g. sudo apt-get install tmux"
	}
}

// linuxFamily reads /etc/os-release and classifies it. Returns "" when the file
// is unreadable; the caller falls back to a generic hint.
func linuxFamily() string {
	data, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	return familyFromOSRelease(string(data))
}

// familyFromOSRelease is the pure classifier behind linuxFamily — a coarse
// ID / ID_LIKE substring match. Split out so it's unit-testable without a real
// /etc/os-release. The `sudo` in the hints linuxTmuxHint builds is part of the
// string the user copy-pastes — cc-fleet itself never elevates.
func familyFromOSRelease(osRelease string) string {
	text := strings.ToLower(osRelease)
	switch {
	case strings.Contains(text, "debian"), strings.Contains(text, "ubuntu"):
		return "debian"
	case strings.Contains(text, "fedora"), strings.Contains(text, "rhel"), strings.Contains(text, "centos"):
		return "fedora"
	case strings.Contains(text, "arch"):
		return "arch"
	case strings.Contains(text, "alpine"):
		return "alpine"
	}
	return ""
}
