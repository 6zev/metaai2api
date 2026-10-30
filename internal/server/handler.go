package server

import (
	"net/http"

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
	return openai.New(openai.Options{Backend: b, APIToken: cfg.APIToken})
}
