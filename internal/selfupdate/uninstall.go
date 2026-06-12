package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/tmux"
)

// npmPackage is the published npm package; `npm uninstall -g` on it removes the
// whole npm-owned tree (binary, launcher, manifest).
const npmPackage = "@ethanhq/cc-fleet"

// execRunner runs the external admin tools UninstallPlugin / UninstallBinary
// shell out to (claude / npm). A package var so tests can stub the exec without
// touching the real system.
var execRunner = runCmd

// UninstallPlugin removes the cc-fleet Claude Code plugin registration: the
// plugin itself plus (best-effort) the marketplace entry the installer added.
// The removal is scoped: an unscoped `claude plugin uninstall` defaults to user
// scope and would leave a project/local-scoped install registered, so the
// scope recorded in the install manifest is passed through (user when no
// manifest recorded one — the installers' default). When the plugin isn't
// cached there is nothing to do; when `claude` is not on PATH the exact
// commands are returned for the user to run instead. Tool output streams to
// out.
func UninstallPlugin(ctx context.Context, out io.Writer) (removed, manual []string) {
	var man manifest
	if exe, err := exePath(); err == nil {
		_, man = detectMethod(exe)
	}
	// The cache glob is only a probe, not the registration. Skip ONLY when the
	// cache is empty AND the install manifest never chose the plugin — a
	// cleared cache must not leave a manifest-recorded registration behind.
	if !pluginInstalled() && man.Skill != "plugin" {
		return nil, nil
	}
	// When no manifest recorded the scope, user (the installers' default) is a
	// GUESS — the exec attempt uses it, but the manual fallback commands show
	// the scope as a choice so a project/local registration isn't missed.
	scope, scopeArg := man.PluginScope, man.PluginScope
	if scope == "" {
		scope, scopeArg = "user", "<user|project|local>"
	}
	// The marketplace removal is scoped for the same reason — and more so: an
	// unscoped `marketplace remove` deletes the declaration from EVERY scope,
	// which could break other installs sharing it.
	pluginCmd := "claude plugin uninstall " + pluginRef + " --scope " + scopeArg
	marketCmd := "claude plugin marketplace remove " + marketplace + " --scope " + scopeArg
	claude, err := exec.LookPath("claude")
	if err != nil {
		return nil, []string{pluginCmd, marketCmd}
	}
	if err := execRunner(ctx, out, claude, "plugin", "uninstall", pluginRef, "--scope", scope); err != nil {
		return nil, []string{pluginCmd, marketCmd}
	}
	removed = append(removed, "plugin "+pluginRef+" ("+scope+" scope)")
	marketOK := execRunner(ctx, out, claude, "plugin", "marketplace", "remove", marketplace, "--scope", scope) == nil
	if marketOK {
		removed = append(removed, "plugin marketplace "+marketplace+" ("+scope+" scope)")
	} else {
		manual = append(manual, marketCmd)
	}
	// When the scope was a guess, verify with the one signal cc-fleet owns:
	// if the cache still shows the plugin after the user-scope attempt, a
	// project/local registration likely remains — hand back scoped commands
	// rather than reporting a false complete.
	if scopeArg != scope && pluginInstalled() {
		manual = append(manual, pluginCmd)
		if marketOK {
			manual = append(manual, marketCmd)
		}
	}
	return removed, manual
}

// UninstallBinary removes the running executable and its co-located install
// artifacts (the `ccf` alias when it resolves to this executable, the install
// manifest, the `.previous` rollback backup), routed by install method:
//
//   - npm owns its tree → run `npm uninstall -g` (or return the command when
//     npm is missing or the run fails);
//   - tarball / go → remove the files directly (on unix an unlinked running
//     binary keeps executing until exit);
//   - unknown method → never guess-delete; return the removal commands for the
//     resolved paths instead.
//
// On windows a running exe cannot be unlinked, so every action is returned as
// a manual command to run after this process exits. Callers run this LAST so
// the rest of the uninstall is already done when the binary goes.
func UninstallBinary(ctx context.Context, out io.Writer) (removed, kept, manual []string) {
	exe, err := exePath()
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("# locate and delete the cc-fleet binary by hand (resolve failed: %v)", err)}
	}
	method, _ := detectMethod(exe)
	arts := binaryArtifacts(exe)

	npmCmd := "npm uninstall -g " + npmPackage
	if runtime.GOOS == "windows" {
		manual = append(manual, "# run after this process exits:")
		if method == MethodNpm {
			return nil, nil, append(manual, npmCmd)
		}
		for _, p := range arts {
			manual = append(manual, fmt.Sprintf("del \"%s\"", p))
		}
		return nil, nil, manual
	}

	switch method {
	case MethodNpm:
		if _, lerr := exec.LookPath("npm"); lerr != nil {
			return nil, nil, []string{npmCmd}
		}
		if err := execRunner(ctx, out, "npm", "uninstall", "-g", npmPackage); err != nil {
			return nil, nil, []string{npmCmd}
		}
		// PATH's npm may serve a different global prefix (nvm/asdf) than the
		// one holding this binary — believe the filesystem, not the exit code.
		if _, serr := os.Lstat(exe); serr == nil {
			kept = append(kept, fmt.Sprintf("%s (still present after npm uninstall — npm on PATH may serve a different prefix)", exe))
			return nil, kept, []string{npmCmd}
		}
		return []string{"npm package " + npmPackage}, nil, nil
	case MethodTarball, MethodGo: // cc-fleet owns the files
		for _, p := range arts {
			if err := os.Remove(p); err != nil {
				kept = append(kept, fmt.Sprintf("%s (remove failed: %v)", p, err))
			} else {
				removed = append(removed, p)
			}
		}
		return removed, kept, nil
	default:
		// MethodUnknown — or a manifest method this binary doesn't recognize
		// (a newer installer, a corrupt file): not provably ours to delete, so
		// hand the resolved paths back instead, shell-quoted as pure data.
		for _, p := range arts {
			manual = append(manual, "rm "+tmux.Quote(p))
		}
		return nil, nil, manual
	}
}

// binaryArtifacts lists the existing install artifacts next to exe: the
// executable itself, the sibling alias, the install manifest, and the rollback
// backup. On unix the alias is the `ccf` symlink (only when it resolves to exe
// — never someone else's ccf); on windows the installers copy the exe to both
// canonical names, so the sibling copy is listed by name (the windows path only
// prints removal commands, never deletes).
func binaryArtifacts(exe string) []string {
	return binaryArtifactsFor(exe, runtime.GOOS)
}

func binaryArtifactsFor(exe, goos string) []string {
	dir := filepath.Dir(exe)
	var arts []string
	add := func(p string) {
		if _, err := os.Lstat(p); err == nil {
			arts = append(arts, p)
		}
	}
	if goos == "windows" {
		for _, name := range []string{"cc-fleet.exe", "ccf.exe"} {
			if !strings.EqualFold(filepath.Base(exe), name) {
				add(filepath.Join(dir, name))
			}
		}
	} else {
		alias := filepath.Join(dir, "ccf")
		if alias != exe {
			if target, err := filepath.EvalSymlinks(alias); err == nil && target == exe {
				add(alias)
			}
		}
	}
	add(filepath.Join(dir, manifestName))
	add(exe + ".previous")
	add(exe) // last so the alias/manifest checks above ran against a live exe
	return arts
}
