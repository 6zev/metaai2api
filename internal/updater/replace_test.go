package updater

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeFakeExecutable drops a file named name in dir with the given content and
// executable bits, standing in for the running binary.
func writeFakeExecutable(t *testing.T, dir, name string, content []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, content, 0o755); err != nil {
		t.Fatalf("write fake executable: %v", err)
	}
	return path
}

// dirEntries returns the base names present in dir.
func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestRenameReplacesFile(t *testing.T) {
	dir := t.TempDir()
	target := writeFakeExecutable(t, dir, "app", []byte("old binary"))

	first := filepath.Join(dir, "app.new1")
	if err := os.WriteFile(first, []byte("new binary one"), 0o755); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := replace(first, target); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new binary one" {
		t.Fatalf("target content = %q, want %q", got, "new binary one")
	}
	if _, err := os.Stat(first); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("source should have been renamed away; stat err = %v", err)
	}

	// A second replace over the result must work too: the rename target is a
	// regular file again, not left in an aside state.
	second := filepath.Join(dir, "app.new2")
	if err := os.WriteFile(second, []byte("new binary two"), 0o755); err != nil {
		t.Fatalf("write second: %v", err)
	}
	if err := replace(second, target); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	got, err = os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target after second: %v", err)
	}
	if string(got) != "new binary two" {
		t.Fatalf("target content = %q, want %q", got, "new binary two")
	}

	// No aside or temp artifact survives a successful replace.
	names := dirEntries(t, dir)
	if len(names) != 1 || names[0] != "app" {
		t.Fatalf("dir after replace = %v, want only [app]", names)
	}
}

func TestReplaceRefusesNonRegularTarget(t *testing.T) {
	dir := t.TempDir()
	src := writeFakeExecutable(t, dir, "app.new", []byte("new binary"))
	target := filepath.Join(dir, "not-a-file")
	if err := os.Mkdir(target, 0o755); err != nil {
		t.Fatalf("mkdir target: %v", err)
	}

	if err := replace(src, target); err == nil {
		t.Fatal("replace over a directory: want error, got nil")
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source must survive a refused replace: %v", err)
	}
}

func TestReplaceRefusesNonExecutableTarget(t *testing.T) {
	dir := t.TempDir()
	src := writeFakeExecutable(t, dir, "app.new", []byte("new binary"))
	target := filepath.Join(dir, "app")
	if err := os.WriteFile(target, []byte("old"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}

	if err := replace(src, target); err == nil {
		t.Fatal("replace over a non-executable: want error, got nil")
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, []byte("old")) {
		t.Fatalf("target changed = %q, want %q", got, "old")
	}
}

func TestWindowsRenameAsideBranch(t *testing.T) {
	dir := t.TempDir()
	aside := filepath.Join(dir, "app.old")
	if err := os.WriteFile(aside, []byte("stale aside"), 0o755); err != nil {
		t.Fatalf("write stale aside: %v", err)
	}
	target := writeFakeExecutable(t, dir, "app", []byte("old binary"))
	newBin := writeFakeExecutable(t, dir, "app.new", []byte("new binary"))

	if err := replaceAside(newBin, target, aside); err != nil {
		t.Fatalf("replaceAside: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if string(got) != "new binary" {
		t.Fatalf("target content = %q, want %q", got, "new binary")
	}

	// The moved-aside old binary replaces any stale aside left by a previous
	// run, and the new binary is gone from its staging path.
	old, err := os.ReadFile(aside)
	if err != nil {
		t.Fatalf("read aside: %v", err)
	}
	if string(old) != "old binary" {
		t.Fatalf("aside content = %q, want %q", old, "old binary")
	}
	if _, err := os.Stat(newBin); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("new binary should have been renamed in; stat err = %v", err)
	}
}

// TestUseAsideFallbackOnlyOnWindows pins the branch predicate: the aside
// strategy is a Windows-only fallback, never a POSIX one.
func TestUseAsideFallbackOnlyOnWindows(t *testing.T) {
	want := runtime.GOOS == "windows"
	if got := useAsideFallback(); got != want {
		t.Fatalf("useAsideFallback() = %v on %s, want %v", got, runtime.GOOS, want)
	}
}
