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

// Secrets masks every non-empty secret in msg with config.RedactedSecret.
//
// Callers should pass the values as configured, not just the ones they believe
// are sensitive: error text is arbitrary, and a URL parser, a JSON decoder or an
// upstream response body may quote any of them.
func Secrets(msg string, secrets ...string) string {
	for _, sec := range secrets {
		sec = strings.TrimSpace(sec)
		if sec == "" {
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
