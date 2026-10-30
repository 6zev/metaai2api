// Package version holds the single source of truth for the build version.
//
// It is set at link time:
//
//	go build -ldflags "-X github.com/me/metaai2api/internal/version.Version=v1.2.3"
//
// The updater compares against it and the server exposes it at /admin/version.
// No other package may define its own version variable.
package version

// Version is the current build version. Overridden via ldflags.
var Version = "dev"
