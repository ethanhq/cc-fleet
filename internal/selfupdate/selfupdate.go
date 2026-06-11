// Package selfupdate updates the cc-fleet binary and its Claude Code plugin to
// the latest GitHub release. It is method-aware: a tarball install self-updates
// in-process (download the prebuilt release, verify sha256, smoke-test, then
// atomically replace the running binary), while an npm or `go install` install
// shells out to the right package manager — falling back to printing the exact
// command only when the tool is missing or the install location is not writable.
//
// Only a comparable release version (version.IsRelease) is ever acted on: a dev
// or VCS-pseudo build is reported as "not comparable" and never updated.
package selfupdate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/config"
	"github.com/ethanhq/cc-fleet/internal/version"
)

const (
	repo        = "ethanhq/cc-fleet"
	marketplace = "ethanhq"
	pluginRef   = "cc-fleet@ethanhq"
	// manifestName is the co-located install record install.sh / npm write so
	// update detects the install method without fragile path heuristics.
	manifestName = ".cc-fleet-install.json"
	// maxAsset bounds a downloaded asset so a hostile/oversized response can't
	// exhaust memory (the binary tarball is ~10-15 MB).
	maxAsset = 128 << 20
)

// githubBase is the GitHub origin; a package var so tests can point it at a
// local httptest server.
var githubBase = "https://github.com"

// Method is how cc-fleet was installed, which decides how to update the binary.
type Method string

const (
	MethodTarball Method = "tarball" // curl|sh / release tarball → in-Go self-update
	MethodNpm     Method = "npm"     // npm -g → `npm install -g`
	MethodGo      Method = "go"      // go install → `go install ...@latest`
	MethodUnknown Method = "unknown"
)

// Status compares the running binary to the latest release.
type Status struct {
	Current        string // version.Resolve()
	Latest         string // latest release tag, "" if unknown
	Comparable     bool   // Current is a release version (not dev/pseudo)
	NewerAvailable bool   // Comparable && Latest is newer than Current
}

// manifest is the co-located install record (next to the binary) install.sh /
// npm write so update knows the install method, the plugin scope to preserve,
// and the skill choice (so a --skill none/global user isn't force-fed a plugin).
type manifest struct {
	Method      string `json:"method"`
	PluginScope string `json:"plugin_scope,omitempty"`
	Skill       string `json:"skill,omitempty"` // plugin | global | none
}

// Options tunes Run.
type Options struct {
	BinaryOnly bool      // skip the plugin update
	Force      bool      // self-update even when the install method is unknown
	Out        io.Writer // progress sink (defaults to os.Stdout)
}

// Check resolves the current version and the latest release tag and compares
// them. A transport error is returned with Status.Current/Comparable still set
// so callers can distinguish "offline" from "dev build".
func Check(ctx context.Context) (Status, error) {
	cur := version.Resolve()
	st := Status{Current: cur, Comparable: version.IsRelease(cur)}
	latest, err := LatestTag(ctx)
	if err != nil {
		return st, err
	}
	st.Latest = latest
	st.NewerAvailable = st.Comparable && version.Newer(latest, cur)
	return st, nil
}

// LatestTag returns the latest release tag (e.g. "v0.1.8") by reading the
// /releases/latest redirect — no API token, no JSON, no rate-limit-prone API.
// A repo with no releases redirects to /releases (no tag) and yields an error.
func LatestTag(ctx context.Context) (string, error) {
	url := fmt.Sprintf("%s/%s/releases/latest", githubBase, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "cc-fleet")
	// Stop at the redirect so we can read its Location instead of following it.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no redirect from %s (HTTP %d)", url, resp.StatusCode)
	}
	tag := path.Base(loc)
	if !version.IsRelease(tag) {
		return "", fmt.Errorf("no release found (latest resolved to %q)", tag)
	}
	return tag, nil
}

// osExecutable is os.Executable, indirected so tests can point the update flow
// at a fake binary instead of the test runner.
var osExecutable = os.Executable

// exePath returns the running binary's real path with symlinks resolved (the
// `ccf` alias is a relative symlink to cc-fleet).
func exePath() (string, error) {
	p, err := osExecutable()
	if err != nil {
		return "", err
	}
	if resolved, rerr := filepath.EvalSymlinks(p); rerr == nil {
		p = resolved
	}
	return p, nil
}

// detectMethod reads the co-located install manifest, falling back to a path
// heuristic. Returns the manifest too so the plugin scope is preserved.
func detectMethod(exe string) (Method, manifest) {
	dir := filepath.Dir(exe)
	if data, err := os.ReadFile(filepath.Join(dir, manifestName)); err == nil {
		var m manifest
		if json.Unmarshal(data, &m) == nil && m.Method != "" {
			return Method(m.Method), m
		}
	}
	// No manifest (older install / go install has no postinstall hook): guess.
	if strings.Contains(exe, string(filepath.Separator)+"node_modules"+string(filepath.Separator)) {
		return MethodNpm, manifest{}
	}
	for _, c := range goBinDirs() {
		if c != "" && dir == filepath.Clean(c) {
			return MethodGo, manifest{}
		}
	}
	return MethodUnknown, manifest{}
}

// goBinDirs lists the directories `go install` would target.
func goBinDirs() []string {
	var dirs []string
	if v := os.Getenv("GOBIN"); v != "" {
		dirs = append(dirs, v)
	}
	if v := os.Getenv("GOPATH"); v != "" {
		dirs = append(dirs, filepath.Join(v, "bin"))
	}
	if h := os.Getenv("HOME"); h != "" {
		dirs = append(dirs, filepath.Join(h, "go", "bin"))
	}
	return dirs
}

// updateLockPath is a single global lock under ConfigDir that serializes
// concurrent `ccf update` runs (a user has one install; cross-install
// serialization is harmless and ConfigDir is always writable, unlike a
// system bin dir).
func updateLockPath() (string, error) {
	dir, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, ".cc-fleet-update.lock"), nil
}

// WithUpdateLock runs fn under the self-update lock — the complete uninstall
// holds it around its plugin/binary removal so a concurrent `ccf update` never
// mutates the same artifacts mid-removal.
func WithUpdateLock(fn func() error) error {
	lockPath, err := updateLockPath()
	if err != nil {
		return err
	}
	return config.WithFlock(lockPath, fn)
}

// Run updates the binary and (unless BinaryOnly) the plugin, serialized by the
// update lock. Order is prepare-binary → update-plugin → commit-binary so a
// plugin failure never leaves binary and plugin at different versions.
func Run(ctx context.Context, opts Options) error {
	out := opts.Out
	if out == nil {
		out = os.Stdout
	}
	if runtime.GOOS == "windows" {
		fmt.Fprintln(out, "cc-fleet self-update is not supported on Windows — reinstall the latest release manually.")
		return nil
	}
	lockPath, err := updateLockPath()
	if err != nil {
		return err
	}
	return config.WithFlock(lockPath, func() error { return runLocked(ctx, opts, out) })
}

func runLocked(ctx context.Context, opts Options, out io.Writer) error {
	exe, err := exePath()
	if err != nil {
		return err
	}
	// A dev/non-release build can't self-update — short-circuit before any
	// network so an offline dev build still gets a clean notice.
	if cur := version.Resolve(); !version.IsRelease(cur) {
		fmt.Fprintf(out, "Development build (%s) — not a release; reinstall to update.\n", cur)
		return nil
	}
	st, err := Check(ctx)
	if err != nil {
		return fmt.Errorf("check for updates: %w", err)
	}

	method, man := detectMethod(exe)

	// 1. Prepare the binary update (no irreversible side effect yet). For a
	//    tarball install this downloads + verifies + smoke-tests into a staged
	//    temp file; commitBinary then renames it in. For npm/go there is no
	//    dry-run, so commitBinary runs the package manager.
	var (
		commitBinary func() error
		stagedPath   string
		committed    bool
	)
	// Remove a staged-but-uncommitted binary on any early return after staging
	// (e.g. the plugin step fails and we abort before the swap).
	defer func() {
		if stagedPath != "" && !committed {
			_ = os.Remove(stagedPath)
		}
	}()
	switch {
	case !st.NewerAvailable:
		fmt.Fprintf(out, "Binary already at the latest (%s).\n", st.Current)
	case method == MethodTarball || (method == MethodUnknown && opts.Force):
		staged, perr := prepareTarballBinary(ctx, exe, st.Latest, out)
		if perr != nil {
			return perr
		}
		stagedPath = staged
		commitBinary = func() error { return swapBinary(exe, staged, st.Current, st.Latest, out) }
	case method == MethodNpm:
		commitBinary = func() error { return updateViaPkgManager(ctx, out, "npm", "install", "-g", "@ethanhq/cc-fleet@latest") }
	case method == MethodGo:
		commitBinary = func() error {
			return updateViaPkgManager(ctx, out, "go", "install", "github.com/ethanhq/cc-fleet/cmd/cc-fleet@latest")
		}
	default: // unknown method, not forced
		fmt.Fprintf(out, "cc-fleet %s is available, but the install method is unknown — reinstall, or rerun `ccf update --force` to self-update in place.\n", st.Latest)
	}

	// 2. Plugin update (independent of the binary; runs before the binary swap).
	if !opts.BinaryOnly {
		if perr := updatePlugin(ctx, man.PluginScope, man.Skill, out); perr != nil {
			if commitBinary != nil {
				return fmt.Errorf("plugin update failed; binary left unchanged to avoid a version skew (rerun with --binary-only to update the binary anyway): %w", perr)
			}
			return perr
		}
	}

	// 3. Commit the binary last.
	if commitBinary != nil {
		if cerr := commitBinary(); cerr != nil {
			return cerr
		}
		committed = true
	}
	return nil
}

// download GETs url (following redirects to the asset CDN) and returns the body,
// failing explicitly if it exceeds maxAsset rather than silently truncating.
func download(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "cc-fleet")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxAsset+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxAsset {
		return nil, fmt.Errorf("GET %s: response exceeds %d bytes", url, maxAsset)
	}
	return data, nil
}

// assetBase is the release-download base for a tag (CCF_BASE_URL overrides it
// for a mirror or a local test, matching install.sh / npm).
func assetBase(tag string) string {
	if b := os.Getenv("CCF_BASE_URL"); b != "" {
		return b
	}
	return fmt.Sprintf("%s/%s/releases/download/%s", githubBase, repo, tag)
}
