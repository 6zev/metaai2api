package server

import (
	"net/http"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/openai"
)

// newV1Handler builds the /v1 handler for cfg. The server never constructs the
// v1 routes itself: it mounts internal/openai unchanged.
func newV1Handler(cfg config.Config) (http.Handler, error) {
	b, err := backendFor(cfg)
	if err != nil {
		return nil, err
	}
	return newV1HandlerFor(cfg, b)
}

// newV1HandlerFor builds the /v1 handler for an explicit backend. It is separate
// from newV1Handler so PUT /admin/config can rebuild the handler for the
// backend it just resolved without resolving it twice.
func newV1HandlerFor(cfg config.Config, b backend.ChatBackend) (http.Handler, error) {
	return openai.New(openai.Options{
		Backend:  b,
		APIToken: cfg.APIToken,
		// Derived from one place so the list cannot drift from what the admin
		// surface scrubs. Includes proxy credentials.
		Secrets: Secrets(cfg),
	})
}
