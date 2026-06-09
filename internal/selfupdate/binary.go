package selfupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/ethanhq/cc-fleet/internal/version"
)

// prepareTarballBinary downloads the release tarball + checksums for this
// os/arch, verifies sha256 (fail-closed), extracts the cc-fleet binary into a
// temp file in the binary's own directory (same filesystem → atomic rename),
// and smoke-tests it. It installs nothing; it returns the staged temp path for
// swapBinary to commit. Any failure removes the staged file and leaves the live
// binary untouched.
func prepareTarballBinary(ctx context.Context, exe, tag string, out io.Writer) (string, error) {
	dir := filepath.Dir(exe)
	if !dirWritable(dir) {
		return "", fmt.Errorf("%s is not writable — reinstall with elevated permissions or from a writable prefix", dir)
	}

	osArch := runtime.GOOS + "-" + runtime.GOARCH
	tarName := fmt.Sprintf("cc-fleet-%s.tar.gz", osArch)
	base := assetBase(tag)

	fmt.Fprintf(out, "  ↓ %s\n", tarName)
	tarBytes, err := download(ctx, base+"/"+tarName)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", tarName, err)
	}
	sumsBytes, err := download(ctx, base+"/checksums.txt")
	if err != nil {
		return "", fmt.Errorf("download checksums.txt: %w", err)
	}
	expected := checksumFor(string(sumsBytes), tarName)
	if expected == "" {
		return "", fmt.Errorf("no checksum for %s in checksums.txt", tarName)
	}
	actual := sha256Hex(tarBytes)
	if actual != expected {
		return "", fmt.Errorf("checksum mismatch for %s", tarName)
	}
	fmt.Fprintln(out, "  ✔ sha256 verified")

	tmp, err := os.CreateTemp(dir, ".cc-fleet-update-*")
	if err != nil {
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	staged := tmp.Name()
	if err := extractBinaryTo(tarBytes, tmp); err != nil {
		_ = tmp.Close()
		_ = os.Remove(staged)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	if err := smokeTest(ctx, staged, tag); err != nil {
		_ = os.Remove(staged)
		return "", err
	}
	fmt.Fprintln(out, "  ✔ smoke test passed")
	return staged, nil
}

// extractBinaryTo writes the `cc-fleet` regular-file entry from the gzip tar to
// w. The goreleaser archive wraps contents in cc-fleet-<os>-<arch>/, so the
// binary is matched by basename.
func extractBinaryTo(tarGz []byte, w io.Writer) error {
	gz, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer func() { _ = gz.Close() }()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return fmt.Errorf("cc-fleet binary not found in tarball")
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}
		if hdr.Typeflag == tar.TypeReg && filepath.Base(hdr.Name) == "cc-fleet" {
			if _, err := io.Copy(w, io.LimitReader(tr, maxAsset)); err != nil {
				return fmt.Errorf("extract cc-fleet: %w", err)
			}
			return nil
		}
	}
}

// smokeTest runs `<staged> --version` and confirms it reports the expected tag,
// so a corrupt or wrong-arch binary is caught before it replaces the live one.
func smokeTest(ctx context.Context, staged, tag string) error {
	out, err := exec.CommandContext(ctx, staged, "--version").Output()
	if err != nil {
		return fmt.Errorf("smoke test: %w", err)
	}
	if !strings.Contains(string(out), version.Normalize(tag)) {
		return fmt.Errorf("smoke test: new binary reported %q, expected %s", strings.TrimSpace(string(out)), tag)
	}
	return nil
}

// swapBinary backs the current binary up to <exe>.previous and renames the
// staged binary into place. On unix a running executable's file can be renamed
// (the live process keeps its open inode); staged, exe, and the backup share a
// directory so every rename is atomic and same-filesystem.
func swapBinary(exe, staged, oldVer, newVer string, out io.Writer) error {
	backup := exe + ".previous"
	if err := os.Rename(exe, backup); err != nil {
		_ = os.Remove(staged)
		return fmt.Errorf("back up current binary: %w", err)
	}
	if err := os.Rename(staged, exe); err != nil {
		_ = os.Rename(backup, exe) // restore
		return fmt.Errorf("install new binary: %w", err)
	}
	fmt.Fprintf(out, "  ✔ binary %s → %s  (%s; previous kept at %s)\n",
		oldVer, newVer, exe, filepath.Base(backup))
	return nil
}

// Rollback restores the <exe>.previous backup left by the last self-update.
func Rollback(out io.Writer) error {
	if out == nil {
		out = os.Stdout
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	backup := exe + ".previous"
	if _, err := os.Stat(backup); err != nil {
		return fmt.Errorf("no previous binary to roll back to (%s)", backup)
	}
	if err := os.Rename(backup, exe); err != nil {
		return fmt.Errorf("restore previous binary: %w", err)
	}
	fmt.Fprintf(out, "rolled back to the previous binary (%s)\n", exe)
	return nil
}

// dirWritable reports whether a file can be created in dir.
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".cc-fleet-writetest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// checksumFor returns the hex sha256 for name from a `<sum>  <name>` listing.
func checksumFor(sums, name string) string {
	for _, line := range strings.Split(sums, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == name {
			return fields[0]
		}
	}
	return ""
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
