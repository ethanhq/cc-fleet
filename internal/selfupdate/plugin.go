package selfupdate

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// updatePlugin brings the Claude Code plugin to the latest version, preserving
// the install scope. A user who installed with --skill none or global declined
// the plugin, so it is left alone. If `claude` is not on PATH the plugin step is
// skipped (not an error — the binary still updates). An installed plugin is
// refreshed via `marketplace update` + `plugin update`; the fallback
// uninstall + reinstall runs ONLY when the scope is known, so an unknown-scope
// install is never silently moved to user scope. An absent plugin is installed.
func updatePlugin(ctx context.Context, scope, skill string, out io.Writer) error {
	if skill == "none" || skill == "global" {
		fmt.Fprintf(out, "  skill installed via --skill %s — leaving the plugin alone\n", skill)
		return nil
	}
	claude, err := exec.LookPath("claude")
	if err != nil {
		fmt.Fprintln(out, "  claude not on PATH — skipping the plugin update")
		return nil
	}

	if pluginInstalled() {
		fmt.Fprintln(out, "  ↻ updating plugin "+pluginRef)
		if err := runCmd(ctx, out, claude, "plugin", "marketplace", "update", marketplace); err != nil {
			return fmt.Errorf("claude plugin marketplace update: %w", err)
		}
		if err := runCmd(ctx, out, claude, "plugin", "update", pluginRef); err == nil {
			return nil
		}
		if scope == "" {
			return fmt.Errorf("plugin update failed and the install scope is unknown — reinstall manually: claude plugin install %s --scope <user|project|local>", pluginRef)
		}
		fmt.Fprintln(out, "  plugin update failed — reinstalling in the "+scope+" scope")
		_ = runCmd(ctx, out, claude, "plugin", "uninstall", pluginRef)
		return runCmd(ctx, out, claude, "plugin", "install", pluginRef, "--scope", scope)
	}

	if scope == "" {
		scope = "user"
	}
	fmt.Fprintln(out, "  ↓ installing plugin "+pluginRef)
	if err := runCmd(ctx, out, claude, "plugin", "marketplace", "add", repo, "--scope", scope); err != nil {
		return fmt.Errorf("claude plugin marketplace add: %w", err)
	}
	return runCmd(ctx, out, claude, "plugin", "install", pluginRef, "--scope", scope)
}

// pluginInstalled reports whether Claude Code has the cc-fleet plugin cached
// (~/.claude/plugins/cache/<marketplace>/cc-fleet/<version>/). Offline + cheap.
func pluginInstalled() bool {
	home := os.Getenv("HOME")
	if home == "" {
		return false
	}
	matches, _ := filepath.Glob(filepath.Join(home, ".claude", "plugins", "cache", "*", "cc-fleet", "*"))
	return len(matches) > 0
}

// updateViaPkgManager runs an installer-managed binary update (npm / go). The
// binary lives in a package-manager-owned tree, so self-replacing it would
// desync the manager — the manager must do the update. If the tool is missing,
// print the command instead of failing the whole update.
func updateViaPkgManager(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmdline := name + " " + strings.Join(args, " ")
	if _, err := exec.LookPath(name); err != nil {
		fmt.Fprintf(out, "  %s not on PATH — update manually:\n    %s\n", name, cmdline)
		return nil
	}
	fmt.Fprintf(out, "  ↻ %s\n", cmdline)
	if err := runCmd(ctx, out, name, args...); err != nil {
		if name == "npm" {
			fmt.Fprintf(out, "  npm failed — if it is a permissions error, try:\n    sudo %s\n", cmdline)
		}
		return fmt.Errorf("%s update: %w", name, err)
	}
	return nil
}

// runCmd runs an external admin tool (claude / npm / go), streaming its output.
// It inherits the user's environment — childenv.Clean is for provider-worker
// children only, not for trusted local tooling.
func runCmd(ctx context.Context, out io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = out
	cmd.Stderr = out
	return cmd.Run()
}
