package config_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/me/metaai2api/internal/config"
)

// writeEnv writes a .env file into a temp dir and returns its path.
func writeEnv(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return p
}

// TestDotEnvSetsValues covers the syntax an operator will actually use.
func TestDotEnvSetsValues(t *testing.T) {
	// Keys are unique to this test so a developer's shell cannot interfere.
	p := writeEnv(t, `
# a comment line
DOTENV_PLAIN=hello
DOTENV_QUOTED="two words"
DOTENV_SINGLE='literal $NOT_EXPANDED'
DOTENV_EXPORT=export-prefix
export DOTENV_EXPORTED=yes
DOTENV_HASHVAL="a#b"
DOTENV_ESCAPE="line1\nline2"
DOTENV_TRAILING=trimmed   
DOTENV_EMPTY=
`)
	for _, k := range []string{
		"DOTENV_PLAIN", "DOTENV_QUOTED", "DOTENV_SINGLE", "DOTENV_EXPORT",
		"DOTENV_EXPORTED", "DOTENV_HASHVAL", "DOTENV_ESCAPE", "DOTENV_TRAILING",
		"DOTENV_EMPTY",
	} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	t.Cleanup(func() {
		for _, k := range []string{
			"DOTENV_PLAIN", "DOTENV_QUOTED", "DOTENV_SINGLE", "DOTENV_EXPORT",
			"DOTENV_EXPORTED", "DOTENV_HASHVAL", "DOTENV_ESCAPE", "DOTENV_TRAILING",
			"DOTENV_EMPTY",
		} {
			os.Unsetenv(k)
		}
	})

	if err := config.LoadDotEnv(p); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}

	want := map[string]string{
		"DOTENV_PLAIN":    "hello",
		"DOTENV_QUOTED":   "two words",
		"DOTENV_SINGLE":   "literal $NOT_EXPANDED",
		"DOTENV_EXPORT":   "export-prefix",
		"DOTENV_EXPORTED": "yes",
		// A "#" inside a quoted value is part of the value: a cookie or token
		// can contain one.
		"DOTENV_HASHVAL":  "a#b",
		"DOTENV_ESCAPE":   "line1\nline2",
		"DOTENV_TRAILING": "trimmed",
		"DOTENV_EMPTY":    "",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// TestDotEnvDoesNotOverrideRealEnv is the precedence rule: a platform-injected
// secret must win over a stray file, otherwise deploying a .env silently
// replaces a credential the operator set deliberately.
func TestDotEnvDoesNotOverrideRealEnv(t *testing.T) {
	t.Setenv("DOTENV_PRECEDENCE", "from-environment")
	p := writeEnv(t, "DOTENV_PRECEDENCE=from-file\n")

	if err := config.LoadDotEnv(p); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := os.Getenv("DOTENV_PRECEDENCE"); got != "from-environment" {
		t.Fatalf("env was overridden by the file: %q", got)
	}
}

// TestDotEnvMissingIsNotAnError keeps a .env optional.
func TestDotEnvMissingIsNotAnError(t *testing.T) {
	if err := config.LoadDotEnv(filepath.Join(t.TempDir(), "nope.env")); err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
}

// TestDotEnvRejectsMalformedLine reports a line that is not KEY=value rather
// than skipping it, because a typo there means a secret silently does not load.
func TestDotEnvRejectsMalformedLine(t *testing.T) {
	p := writeEnv(t, "NOT_A_PAIR\n")
	if err := config.LoadDotEnv(p); err == nil {
		t.Fatal("a malformed line should be an error")
	}
}

// TestDotEnvLongValue covers a long cookie: the scanner buffer must not cut it.
func TestDotEnvLongValue(t *testing.T) {
	long := make([]byte, 200_000)
	for i := range long {
		long[i] = 'a'
	}
	p := writeEnv(t, "DOTENV_LONG="+string(long)+"\n")
	t.Cleanup(func() { os.Unsetenv("DOTENV_LONG") })
	os.Unsetenv("DOTENV_LONG")

	if err := config.LoadDotEnv(p); err != nil {
		t.Fatalf("LoadDotEnv: %v", err)
	}
	if got := len(os.Getenv("DOTENV_LONG")); got != len(long) {
		t.Fatalf("long value truncated: %d of %d bytes", got, len(long))
	}
}
