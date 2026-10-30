// Package redact removes configured secrets from outbound text.
//
// It exists as its own package so that every surface that writes text derived
// from configuration or from an upstream (the admin handlers, the OpenAI
// handlers, the updater's error paths and the startup log) shares one
// implementation. Re-implementing scrubbing per surface is how one of them
// quietly ends up unprotected.
package redact

import (
	"strings"

	"github.com/me/metaai2api/internal/config"
)

// MinSecretLen is the shortest value treated as a secret.
//
// Substring replacement has no notion of a word boundary, so a one- or
// two-character "secret" (a short proxy password, say) would be spliced into the
// middle of every unrelated word: "chat failed" would become "ch***t f***iled".
// Scrubbing that corrupts the message it is meant to protect is worse than not
// scrubbing, because the operator can no longer read the error.
//
// Four is low enough to cover realistic tokens and high enough that an
// accidental match inside ordinary prose is implausible.
const MinSecretLen = 4

// Secrets masks every secret in msg with config.RedactedSecret.
//
// Values shorter than MinSecretLen and values equal to the mask itself are
// ignored: the first would corrupt unrelated text, the second is a no-op that
// would only add noise. Callers should pass the values as configured, not just
// the ones they believe are sensitive: error text is arbitrary, and a URL
// parser, a JSON decoder or an upstream response body may quote any of them.
func Secrets(msg string, secrets ...string) string {
	for _, sec := range secrets {
		sec = strings.TrimSpace(sec)
		if !isScrubbable(sec) {
			continue
		}
		msg = strings.ReplaceAll(msg, sec, config.RedactedSecret)
	}
	return msg
}

// Error is Secrets applied to err's text. A nil error yields "".
func Error(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	return Secrets(err.Error(), secrets...)
}

// isScrubbable reports whether a value is worth replacing.
func isScrubbable(s string) bool {
	if len(s) < MinSecretLen {
		return false
	}
	return s != config.RedactedSecret
}

// Usable reports how many of the given values will actually be scrubbed. It
// exists so a caller can warn at startup that a configured secret is too short
// to protect, rather than discovering it from a corrupted error message.
func Usable(secrets ...string) int {
	n := 0
	for _, s := range secrets {
		if isScrubbable(strings.TrimSpace(s)) {
			n++
		}
	}
	return n
}
