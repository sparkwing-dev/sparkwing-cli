// `sparkwing update` -- self-update the CLI binary.
// `sparkwing version update` -- self-update CLI or bump SDK pin.
//
// Top-level shape (binary-only, the common human fast-path):
//
//	sparkwing update [--check] [--force] [--version vX.Y.Z]
//
//	  --check    Report current vs latest; exit 0 if up to date, 1 if behind.
//	  --force    Allow downgrading to an older release.
//	  --version  Specific release to install (default: latest).
//
// Binary download flow mirrors install.sh exactly: fetch the
// version pointer at sparkwing.dev/releases/latest, pull the
// matching tarball, verify its SHA256 against the published
// SHA256SUMS, untar, atomic rename. On macOS the new binary is
// re-signed ad-hoc to avoid SIGKILL on first run (the kernel's
// arm64 enforcement rejects unsigned mach-O binaries).
//
// Installed version is read from runtime/debug.ReadBuildInfo so
// we skip the download when the release already matches. For dev
// builds (`go install ./cmd/sparkwing`) BuildInfo reports "(devel)"
// and the skip check is disabled -- the update always runs unless
// --check is also set.
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	flag "github.com/spf13/pflag"
	"golang.org/x/mod/semver"
)

const (
	updateRepo    = "koreyGambill/sparkwing"
	updateBaseURL = "https://sparkwing.dev"
)

// runUpdate implements `sparkwing update` -- the top-level binary
// self-update verb. This is the human fast-path; it only updates
// the running CLI binary (not the SDK). For SDK updates, see
// `sparkwing version update --sdk`.
func runUpdate(args []string) error {
	fs := flag.NewFlagSet(cmdUpdate.Path, flag.ContinueOnError)
	check := fs.Bool("check", false, "report current vs latest; exit 1 if a newer release exists")
	force := fs.Bool("force", false, "allow downgrading to an older release")
	version := fs.String("version", "", "target release tag (e.g. v0.17.0). Default: latest.")
	if err := parseAndCheck(cmdUpdate, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("update: unexpected positional %q", fs.Arg(0))
	}
	if *check {
		return runUpdateCheck()
	}
	return runUpdateBinary(*version, *force)
}

// runUpdateCheck reports the installed version vs the latest published
// release. Exits 0 when already current, 1 when a newer release exists.
func runUpdateCheck() error {
	current := installedVersion()
	latest, err := fetchLatestRelease()
	if err != nil {
		return fmt.Errorf("update --check: could not fetch latest version: %w", err)
	}

	switch {
	case current == "(devel)" || current == "(unknown)":
		fmt.Fprintf(os.Stdout, "installed: %s (dev build, cannot compare)\n", current)
		fmt.Fprintf(os.Stdout, "latest:    %s\n", latest)
		return nil
	case semver.Compare(current, latest) >= 0:
		fmt.Fprintf(os.Stdout, "sparkwing %s is up to date (latest: %s)\n", current, latest)
		return nil
	default:
		fmt.Fprintf(os.Stdout, "sparkwing %s is behind -- latest is %s\n", current, latest)
		fmt.Fprintf(os.Stdout, "run: sparkwing update\n")
		return exitErrorf(1, "newer version available: %s (installed: %s)", latest, current)
	}
}

// runUpdateBinary resolves the target version, enforces downgrade
// safety, then downloads + verifies + atomically installs the binary.
// Falls back to `go install` when the tarball download fails.
func runUpdateBinary(version string, force bool) error {
	resolved := strings.TrimSpace(version)
	if resolved == "" {
		v, err := fetchLatestRelease()
		if err != nil {
			fmt.Fprintf(os.Stderr, "update: could not fetch latest version (%v); falling back to go install\n", err)
			return updateCLIViaGoInstall("latest")
		}
		resolved = v
	}

	current := installedVersion()

	// Up-to-date check: skip if already at the target.
	if current != "(unknown)" && current != "(devel)" && resolved == current {
		fmt.Fprintf(os.Stdout, "sparkwing is already at %s\n", current)
		return nil
	}

	// Downgrade safety: if both are valid semver and the target is
	// older than the installed version, require --force to proceed.
	if !force && isSemver(current) && isSemver(resolved) {
		if semver.Compare(resolved, current) < 0 {
			return fmt.Errorf(
				"update: %s is older than the installed %s\n  to downgrade, re-run with --force",
				resolved, current,
			)
		}
	}

	currentBin, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate current binary: %w", err)
	}
	currentBin, _ = filepath.EvalSymlinks(currentBin)

	fmt.Fprintf(os.Stdout, "updating sparkwing: %s -> %s\n", current, resolved)

	if err := downloadAndInstall(resolved, currentBin); err != nil {
		fmt.Fprintf(os.Stderr, "update: download path failed (%v); falling back to go install\n", err)
		return updateCLIViaGoInstall(resolved)
	}

	fmt.Fprintf(os.Stdout, "sparkwing updated: %s -> %s\n", current, resolved)
	fmt.Fprintf(os.Stdout, "what's new: https://sparkwing.dev/CHANGELOG.md\n")
	return nil
}

// runVersionUpdate is the unified update dispatcher under
// `sparkwing version update`. Requires exactly one of --cli or --sdk
// so a stray `version update` can't silently replace the wrong half.
func runVersionUpdate(args []string) error {
	fs := flag.NewFlagSet(cmdVersionUpdate.Path, flag.ContinueOnError)
	cli := fs.Bool("cli", false, "self-update the sparkwing CLI binary")
	sdk := fs.Bool("sdk", false, "bump the SDK pin in this project's .sparkwing/go.mod")
	version := fs.String("version", "", "target release (e.g. v0.17.0). Default: latest.")
	force := fs.Bool("force", false, "allow downgrading to an older release")
	if err := parseAndCheck(cmdVersionUpdate, fs, args); err != nil {
		if errors.Is(err, errHelpRequested) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("version update: unexpected positional %q", fs.Arg(0))
	}
	switch {
	case *cli && *sdk:
		return errors.New("version update: --cli and --sdk are mutually exclusive")
	case *cli:
		return runUpdateBinary(*version, *force)
	case *sdk:
		return runUpdateSDK(*version)
	default:
		return errors.New("version update: must pass --cli (binary) or --sdk (per-project go.mod pin)")
	}
}

// installedVersion returns the CLI version the running binary was
// built from. Uses runtime/debug.ReadBuildInfo's Main.Version field,
// which is the module version when built via `go install
// github.com/...@vX.Y.Z` and "(devel)" for local builds.
func installedVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" {
			return v
		}
	}
	return "(unknown)"
}

// downloadAndInstall pulls the binary tarball + SHA256SUMS for the
// given version, verifies the checksum, untars to a sibling tmpfile,
// and atomically renames over currentBin. On macOS the new binary
// is ad-hoc-codesigned so the kernel doesn't SIGKILL it on first
// run (an arm64 mach-O quirk).
func downloadAndInstall(version, currentBin string) error {
	suffix := runtime.GOOS + "-" + runtime.GOARCH
	// Windows publishes .zip (PowerShell can Expand-Archive natively);
	// every other platform publishes .tar.gz.
	archiveExt := ".tar.gz"
	if runtime.GOOS == "windows" {
		archiveExt = ".zip"
	}
	archiveName := "sparkwing-" + suffix + archiveExt
	base := updateBaseURL + "/releases/" + version

	tmpDir, err := os.MkdirTemp("", "sparkwing-update-")
	if err != nil {
		return fmt.Errorf("mkdir tmp: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Download archive + checksum manifest.
	archivePath := filepath.Join(tmpDir, archiveName)
	if err := downloadFile(base+"/"+archiveName, archivePath); err != nil {
		return fmt.Errorf("download %s: %w", archiveName, err)
	}
	sumsPath := filepath.Join(tmpDir, "SHA256SUMS")
	if err := downloadFile(base+"/SHA256SUMS", sumsPath); err != nil {
		return fmt.Errorf("download SHA256SUMS: %w", err)
	}

	// Verify checksum. SHA256SUMS lines are "<sha256>  <filename>";
	// pull the line for our archive and compare against the local
	// digest. A "skip on missing" path here would be a supply-chain
	// foot-gun -- if the manifest is missing or stale, hard-fail.
	expected, err := lookupSHA256(sumsPath, archiveName)
	if err != nil {
		return err
	}
	actual, err := sha256OfFile(archivePath)
	if err != nil {
		return err
	}
	if !strings.EqualFold(expected, actual) {
		return fmt.Errorf("checksum mismatch for %s\n  expected: %s\n  actual:   %s", archiveName, expected, actual)
	}

	// Extract -- the archive contains a single `sparkwing` (or
	// `sparkwing.exe` on Windows) entry at the root.
	extractDir := filepath.Join(tmpDir, "extract")
	if err := os.MkdirAll(extractDir, 0o755); err != nil {
		return err
	}
	if err := extractSparkwing(archivePath, extractDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	binaryName := "sparkwing"
	if runtime.GOOS == "windows" {
		binaryName = "sparkwing.exe"
	}
	newBin := filepath.Join(extractDir, binaryName)
	if _, err := os.Stat(newBin); err != nil {
		return fmt.Errorf("archive did not contain a %s binary at root: %w", binaryName, err)
	}
	if err := os.Chmod(newBin, 0o755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Atomic install: rename onto currentBin. We move via a tmp
	// neighbor of currentBin first so the rename is on the same
	// filesystem (cross-fs renames fail with EXDEV on Linux).
	stagedBin := currentBin + ".update.tmp"
	if err := copyFile(newBin, stagedBin); err != nil {
		return fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Chmod(stagedBin, 0o755); err != nil {
		_ = os.Remove(stagedBin)
		return err
	}

	// macOS ad-hoc codesign so the new binary doesn't get SIGKILL'd
	// on first run. Quietly best-effort: if codesign isn't on PATH
	// or the binary is already signed, ignore the error.
	if runtime.GOOS == "darwin" {
		_ = exec.Command("codesign", "--force", "--sign", "-", stagedBin).Run()
	}

	if err := replaceRunningBinary(stagedBin, currentBin); err != nil {
		_ = os.Remove(stagedBin)
		return fmt.Errorf("replace binary: %w", err)
	}
	return nil
}

// replaceRunningBinary swaps stagedBin into currentBin's place. On POSIX
// this is a single atomic rename. On Windows the running .exe holds an
// exclusive lock that blocks rename-over, so we rename the current binary
// out of the way first (Windows allows renaming a running .exe, just not
// overwriting it), then move the new binary into place. The old binary
// stays on disk as <currentBin>.old until cleanupStaleUpdate runs at
// next startup -- it can't be deleted while it's still executing.
func replaceRunningBinary(stagedBin, currentBin string) error {
	if runtime.GOOS != "windows" {
		return os.Rename(stagedBin, currentBin)
	}
	oldBin := currentBin + ".old"
	_ = os.Remove(oldBin) // best-effort: previous .old from an earlier update
	if err := os.Rename(currentBin, oldBin); err != nil {
		return fmt.Errorf("move running binary aside: %w", err)
	}
	if err := os.Rename(stagedBin, currentBin); err != nil {
		// Try to roll back so the user isn't left with no binary.
		_ = os.Rename(oldBin, currentBin)
		return fmt.Errorf("install new binary: %w", err)
	}
	return nil
}

// cleanupStaleUpdate removes a leftover <self>.old file from a previous
// Windows self-update. The old binary can't be deleted while it's running,
// so deletion is deferred to the first launch of the new binary. Best-effort:
// silent on failure (file might still be locked if multiple sparkwing
// processes are racing; the next launch will retry).
func cleanupStaleUpdate() {
	if runtime.GOOS != "windows" {
		return
	}
	self, err := os.Executable()
	if err != nil {
		return
	}
	_ = os.Remove(self + ".old")
}

// downloadFile fetches url to dst with a 60s timeout. Failures
// surface as wrapped errors -- the caller decides whether to fall
// back to go install or hard-error.
func downloadFile(url, dst string) error {
	client := &http.Client{Timeout: 60 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}

// lookupSHA256 parses a published SHA256SUMS file (lines of
// "<digest>  <filename>") and returns the digest for filename.
// Errors when the line is absent so callers can hard-fail without
// silently skipping the checksum.
func lookupSHA256(sumsPath, filename string) (string, error) {
	body, err := os.ReadFile(sumsPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == filename {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("%s not listed in SHA256SUMS", filename)
}

func sha256OfFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// extractSparkwing dispatches to the right extractor based on file
// extension. Windows publishes .zip; everything else publishes .tar.gz.
func extractSparkwing(archivePath, destDir string) error {
	if strings.HasSuffix(archivePath, ".zip") {
		return unzipSparkwing(archivePath, destDir)
	}
	return untarSparkwing(archivePath, destDir)
}

// untarSparkwing extracts the sparkwing binary from a published
// tarball into destDir. Permissive about other entries (in case
// future tarballs ship a man page, license, etc.) but only the
// regular files matter here.
func untarSparkwing(tarballPath, destDir string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		// Strip any leading directory component so a tarball with
		// or without a top-level dir works.
		name := filepath.Base(hdr.Name)
		out, err := os.Create(filepath.Join(destDir, name))
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, tr); err != nil {
			out.Close()
			return err
		}
		out.Close()
	}
	return nil
}

// unzipSparkwing is the Windows counterpart to untarSparkwing: extracts
// every regular file from a zip archive into destDir, flattening any
// leading directory components. Skips directories and zero-byte entries.
func unzipSparkwing(zipPath, destDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, zf := range r.File {
		if zf.FileInfo().IsDir() {
			continue
		}
		name := filepath.Base(zf.Name)
		rc, err := zf.Open()
		if err != nil {
			return err
		}
		out, err := os.Create(filepath.Join(destDir, name))
		if err != nil {
			rc.Close()
			return err
		}
		if _, err := io.Copy(out, rc); err != nil {
			rc.Close()
			out.Close()
			return err
		}
		rc.Close()
		out.Close()
	}
	return nil
}

// copyFile is os.Rename's portable alternative when src and dst
// might be on different filesystems (e.g., cross-mount tmpdir).
// We still rename within currentBin's filesystem so the final
// install is atomic; this is just for staging.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}

// updateCLIViaGoInstall is the fallback when the tarball download
// fails (release missing, network unavailable). Works for anyone
// with Go toolchain access to this repo via GOPROXY.
func updateCLIViaGoInstall(version string) error {
	target := "github.com/" + updateRepo + "/cmd/sparkwing@"
	if version == "" || version == "latest" {
		target += "latest"
	} else {
		target += version
	}
	fmt.Fprintf(os.Stdout, "go install -ldflags=\"-s -w\" -trimpath %s\n", target)
	cmd := exec.Command("go", "install", "-ldflags=-s -w", "-trimpath", target)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go install: %w", err)
	}
	fmt.Fprintf(os.Stdout, "sparkwing updated via go install -> %s\n", version)
	return nil
}

func runUpdateSDK(version string) error {
	dir, err := findSparkwingDir()
	if err != nil {
		return err
	}

	v := strings.TrimSpace(version)
	if v == "" {
		v = "latest"
	}
	target := "github.com/" + updateRepo + "@" + v
	fmt.Fprintf(os.Stdout, "bumping pipeline SDK to %s\n", v)

	cmd := exec.Command("go", "get", target)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go get: %w", err)
	}

	// Print the resolved version from go.mod so operators see exactly
	// what they got (useful when --version was "latest" or empty).
	if gomod, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		for _, line := range strings.Split(string(gomod), "\n") {
			line = strings.TrimSpace(line)
			if strings.Contains(line, updateRepo) && !strings.HasPrefix(line, "module") {
				parts := strings.Fields(line)
				if len(parts) >= 2 {
					fmt.Fprintf(os.Stdout, "SDK: %s\n", parts[len(parts)-1])
				}
			}
		}
	}

	tidy := exec.Command("go", "mod", "tidy")
	tidy.Dir = dir
	tidy.Stdout = os.Stdout
	tidy.Stderr = os.Stderr
	if err := tidy.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}
	fmt.Fprintln(os.Stdout, "done")
	return nil
}
