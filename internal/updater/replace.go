// Package updater implements the OTA check and self-replace path: query the
// GitHub Releases API, download the matching asset beside the running binary,
// verify its SHA256 against checksums.txt, then rename it over the target.
//
// The package fails closed. The running binary is replaced only after a
// SHA256 from checksums.txt matched the downloaded bytes; every other outcome
// (missing checksums, download failure, mismatch, unusable target) leaves the
// existing binary byte-for-byte intact and removes the temporary file.
package updater

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// ErrTargetNotRegular reports a replace target that is missing, not a regular
// file, or not executable. The updater never writes over such a path.
var ErrTargetNotRegular = errors.New("updater: target is not a regular executable file")

// replace moves src over target, replacing target atomically on POSIX.
//
// src must live in the same directory as target so the final rename stays on
// one filesystem; the caller guarantees that and fsyncs src first. On POSIX the
// rename overwrites target in a single step, so no window exists in which
// target is absent or partial. On Windows the replace is unusable while the
// running image is mapped, so this returns the error and the caller retries
// through replaceAside.
func replace(src, target string) error {
	st, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("updater: stat target: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrTargetNotRegular, target)
	}
	if st.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: %s is not executable", ErrTargetNotRegular, target)
	}
	return renameInto(src, target)
}

// renameInto is the bare os.Rename, split out so tests can exercise the
// rename itself without the target checks.
func renameInto(src, target string) error {
	if err := os.Rename(src, target); err != nil {
		return fmt.Errorf("updater: rename over target: %w", err)
	}
	return nil
}

// asideSuffix names the previous binary kept beside target on Windows.
const asideSuffix = ".old"

// useAsideFallback reports whether a failed direct rename should be retried
// with the rename-aside strategy. That is only correct on Windows, where a
// running image cannot be replaced but can be renamed. Kept as a variable-like
// function so the type is trivially testable.
func useAsideFallback() bool { return runtime.GOOS == "windows" }

// replaceAside implements the Windows strategy: rename the current binary out
// of the way, then move the new one in.
//
// A running Windows image cannot be overwritten, but it can be renamed, so the
// old binary becomes aside and the new one takes its path. Any aside left by an
// earlier run is removed first, because Windows refuses to rename onto an
// existing file. If moving the new binary in fails, the old binary is renamed
// back so target is never left missing.
func replaceAside(src, target, aside string) error {
	st, err := os.Lstat(target)
	if err != nil {
		return fmt.Errorf("updater: stat target: %w", err)
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%w: %s is not a regular file", ErrTargetNotRegular, target)
	}

	if rerr := os.Remove(aside); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		return fmt.Errorf("updater: clear stale aside %s: %w", aside, rerr)
	}
	if err := os.Rename(target, aside); err != nil {
		return fmt.Errorf("updater: rename target aside: %w", err)
	}
	if err := os.Rename(src, target); err != nil {
		// Put the old binary back so the path keeps pointing at a binary.
		if backErr := os.Rename(aside, target); backErr != nil {
			return fmt.Errorf("updater: rename new binary in (%v); restore old binary failed too: %w", err, backErr)
		}
		return fmt.Errorf("updater: rename new binary in: %w", err)
	}
	return nil
}

// sameDirTempPrefix is the prefix of the staging file created next to target.
const tempPrefix = ".updater-"

// createStaging flushes body into a new temporary file in target's directory,
// syncs it to disk and returns its path. The caller owns removing it.
func createStaging(target string, body io.Reader) (string, error) {
	dir := filepath.Dir(target)
	f, err := os.CreateTemp(dir, tempPrefix+"*.tmp")
	if err != nil {
		return "", fmt.Errorf("updater: create staging file: %w", err)
	}
	name := f.Name()
	cleanup := func() {
		f.Close()
		os.Remove(name)
	}

	if _, err := io.Copy(f, body); err != nil {
		cleanup()
		return "", fmt.Errorf("updater: write staging file: %w", err)
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return "", fmt.Errorf("updater: sync staging file: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("updater: close staging file: %w", err)
	}
	if err := os.Chmod(name, 0o755); err != nil {
		os.Remove(name)
		return "", fmt.Errorf("updater: chmod staging file: %w", err)
	}
	return name, nil
}

// countStaged returns the size of the staging file on disk, after the writer
// closed it, so the download limit is checked against what was actually
// persisted.
func countStaged(path string) (int64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, fmt.Errorf("updater: stat staging file: %w", err)
	}
	return st.Size(), nil
}
