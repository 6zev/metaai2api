package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta"
)

// sessionTTL bounds the upstream probe. A cookie check that hangs is worse than
// one that reports failure: the operator is usually already debugging.
const sessionTTL = 15 * time.Second

// handleSession is an operator diagnostic: it runs one cheap persisted query
// against meta.ai and reports whether the configured cookie still works.
//
// It exists so the GraphQL client is reachable from the running binary rather
// than being code only tests exercise. It deliberately does NOT accept a
// caller-chosen doc_id: an arbitrary-document replay endpoint would turn the
// process into an authenticated proxy into meta.ai for anyone holding the admin
// token.
//
// Upstream bodies are never echoed. Only a classification and a scrubbed
// message are returned, because a body may quote the cookie back.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeJSON(w, http.StatusMethodNotAllowed, jsonError("method not allowed"))
		return
	}
	cfg := s.Config()

	ctx, cancel := context.WithTimeout(r.Context(), sessionTTL)
	defer cancel()

	result := map[string]any{
		"endpoint":  meta.DefaultEndpoint,
		"operation": meta.DocEctoHomePromptRouteQuery,
	}

	start := time.Now()
	client, err := meta.New(meta.Options{Cookie: cfg.Cookie})
	if err != nil {
		result["ok"] = false
		result["reason"] = "not_configured"
		result["message"] = s.redact(err.Error())
		result["elapsed_ms"] = time.Since(start).Milliseconds()
		writeJSON(w, http.StatusOK, result)
		return
	}

	_, err = client.GraphQL(ctx, meta.DocEctoHomePromptRouteQuery, map[string]any{"id": "probe"})
	result["ok"] = err == nil
	result["elapsed_ms"] = time.Since(start).Milliseconds()
	if err != nil {
		result["reason"] = classifySessionError(err)
		result["message"] = s.redact(err.Error())
	}
	writeJSON(w, http.StatusOK, result)
}

// classifySessionError maps an upstream failure to one operator-facing cause
// without exposing the underlying text.
func classifySessionError(err error) string {
	switch {
	case errors.Is(err, backend.ErrSessionExpired):
		return "session_expired"
	case isTimeout(err):
		return "timeout"
	case errors.Is(err, backend.ErrUpstreamProtocol):
		return "upstream_error"
	default:
		return "error"
	}
}

func isTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
