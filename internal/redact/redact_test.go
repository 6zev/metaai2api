package redact_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/redact"
)

// TestShortSecretDoesNotCorruptMessage is the regression for a real bug: a
// one-character proxy password became a secret, and substring replacement then
// spliced the mask into every unrelated word.
func TestShortSecretDoesNotCorruptMessage(t *testing.T) {
	msg := "chat failed: chat completions are unavailable"

	got := redact.Secrets(msg, "a", "u:a", "ab", "abc")

	if got != msg {
		t.Fatalf("short secrets corrupted the message:\n got: %q\nwant: %q", got, msg)
	}
}

// TestSecretAtFloorIsScrubbed pins the boundary from the other side, so the
// guard cannot be tightened into uselessness.
func TestSecretAtFloorIsScrubbed(t *testing.T) {
	got := redact.Secrets("token abcd end", "abcd")
	if strings.Contains(got, "abcd") {
		t.Fatalf("secret of exactly MinSecretLen was not scrubbed: %q", got)
	}
	if !strings.Contains(got, config.RedactedSecret) {
		t.Fatalf("no mask inserted: %q", got)
	}
}

func TestSecretsMasksEveryOccurrence(t *testing.T) {
	got := redact.Secrets("k=SECRET-1;k=SECRET-1", "SECRET-1")
	if strings.Contains(got, "SECRET-1") {
		t.Fatalf("not every occurrence was masked: %q", got)
	}
}

// TestMaskItselfIsIgnored stops the mask from being re-masked, which would
// produce a longer, meaningless run of asterisks.
func TestMaskItselfIsIgnored(t *testing.T) {
	got := redact.Secrets("value "+config.RedactedSecret, config.RedactedSecret)
	if got != "value "+config.RedactedSecret {
		t.Fatalf("mask was re-masked: %q", got)
	}
}

func TestWhitespaceIsTrimmed(t *testing.T) {
	got := redact.Secrets("token SECRET-9 here", "  SECRET-9  ")
	if strings.Contains(got, "SECRET-9") {
		t.Fatalf("padded secret was not scrubbed: %q", got)
	}
}

func TestErrorNilYieldsEmpty(t *testing.T) {
	if got := redact.Error(nil, "secret-value"); got != "" {
		t.Fatalf("nil error = %q, want empty", got)
	}
}

func TestErrorScrubsWrappedText(t *testing.T) {
	err := errors.New("dial: SECRET-42 refused")
	got := redact.Error(err, "SECRET-42")
	if strings.Contains(got, "SECRET-42") {
		t.Fatalf("error text leaked: %q", got)
	}
	if !strings.Contains(got, "refused") {
		t.Fatalf("error text lost its meaning: %q", got)
	}
}

// TestUsableCountsOnlyScrubbable pins the startup-warning helper: an operator
// with a two-character proxy password should be told it will not be scrubbed.
func TestUsableCountsOnlyScrubbable(t *testing.T) {
	cases := []struct {
		name    string
		secrets []string
		want    int
	}{
		{"all long", []string{"aaaa", "bbbb"}, 2},
		{"too short", []string{"a", "ab", "abc"}, 0},
		{"empty", []string{"", "   "}, 0},
		{"mask", []string{config.RedactedSecret}, 0},
		{"mixed", []string{"a", "long-enough"}, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redact.Usable(tc.secrets...); got != tc.want {
				t.Fatalf("Usable(%v) = %d, want %d", tc.secrets, got, tc.want)
			}
		})
	}
}
