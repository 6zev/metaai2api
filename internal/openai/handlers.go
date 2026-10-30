package openai

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	backend "github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/redact"
)

// Options configures the /v1 handler.
type Options struct {
	// Backend is the upstream chat implementation. Required.
	Backend backend.ChatBackend
	// APIToken enables the Bearer check. Empty means /v1 is open; this is
	// documented and does not affect the admin routes, which are locked by
	// their own token in internal/server.
	APIToken string
	// Now overrides the clock used for created timestamps in tests. Optional.
	Now func() time.Time
	// Secrets are scrubbed from every error message this handler writes. The
	// server passes the configured cookie, access token and API tokens so an
	// upstream that echoes one back cannot leak it to a client.
	Secrets []string
}

// Handler serves the /v1 routes.
type Handler struct {
	backend  backend.ChatBackend
	apiToken string
	now      func() time.Time
	secrets  []string
}

// New returns the /v1 handler ready to mount. It rejects a nil backend: a
// handler that silently fails every request is worse than refusing to start.
func New(opts Options) (*Handler, error) {
	if opts.Backend == nil {
		return nil, errors.New("openai: backend is required")
	}
	secrets := make([]string, 0, len(opts.Secrets)+1)
	secrets = append(secrets, opts.Secrets...)
	if opts.APIToken != "" {
		secrets = append(secrets, opts.APIToken)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Handler{backend: opts.Backend, apiToken: opts.APIToken, now: now, secrets: secrets}, nil
}

// allowedMethods writes a 405 unless the request method is one of want.
func allowedMethods(w http.ResponseWriter, r *http.Request, want ...string) bool {
	for _, m := range want {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(want, ", "))
	writeError(w, http.StatusMethodNotAllowed, errTypeInvalidRequest, fmt.Sprintf("method %s not allowed", r.Method))
	return false
}

// ServeHTTP routes the /v1 surface. It is mounted under a mux that owns the
// prefix, so paths here are absolute and unknown ones are 404.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/chat/completions":
		h.chatCompletions(w, r)
	case "/v1/models":
		h.models(w, r)
	case "/v1/images/generations":
		h.images(w, r)
	default:
		writeError(w, http.StatusNotFound, errTypeNotFound, fmt.Sprintf("unknown path %s", r.URL.Path))
	}
}

func (h *Handler) chatCompletions(w http.ResponseWriter, r *http.Request) {
	if !allowedMethods(w, r, http.MethodPost) {
		return
	}
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, errTypeAuth, "invalid or missing API token")
		return
	}

	req, err := decodeChatRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest, err.Error())
		return
	}

	agentType, ok := ModelAgentType(req.Model)
	if !ok {
		writeError(w, http.StatusBadRequest, errTypeInvalidRequest,
			fmt.Sprintf("unknown model %q; supported models: %s, %s", req.Model, ModelThinkFast, ModelThinkHard))
		return
	}

	upstream := backend.ChatRequest{
		Messages: []backend.Message{{
			Role:    "user",
			Content: FlattenPrompt(req.Messages),
		}},
		Model:          agentType,
		Stream:         req.Stream,
		ConversationID: req.ConversationID,
	}

	id := "chatcmpl-" + randomID()
	created := h.now().Unix()

	if req.Stream {
		h.stream(w, r, id, created, req.Model, upstream)
		return
	}

	res, err := h.backend.Complete(r.Context(), upstream)
	if err != nil {
		h.writeBackendError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ChatCompletionResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   req.Model,
		Choices: []Completion{{
			Index:        0,
			Message:      Message{Role: "assistant", Content: Content(res.Text)},
			FinishReason: "stop",
		}},
	})
}

// stream writes the SSE stream. Every chunk is flushed immediately so the
// client sees it before the upstream produces the next one; nothing is
// buffered here.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, id string, created int64, model string, upstream backend.ChatRequest) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errTypeServer, "streaming unsupported by this server")
		return
	}
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-cache")
	hdr.Set("Connection", "keep-alive")
	// Defeat proxies that could hold the stream back; harmless otherwise.
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	emitChunk := func(delta ChunkDelta, finish *string) error {
		chunk := ChatCompletionChunk{
			ID:      id,
			Object:  "chat.completion.chunk",
			Created: created,
			Model:   model,
			Choices: []ChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		if err := writeEvent(w, chunk); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	// The first chunk carries the assistant role, as OpenAI clients expect.
	if err := emitChunk(ChunkDelta{Role: "assistant"}, nil); err != nil {
		return
	}

	var streamErr error
	err := h.backend.Stream(r.Context(), upstream, func(d backend.Delta) error {
		if d.Err != nil {
			streamErr = d.Err
			return errStreamAbort
		}
		if d.Text != "" {
			if err := emitChunk(ChunkDelta{Content: Content(d.Text)}, nil); err != nil {
				return err
			}
		}
		if d.Done {
			stop := "stop"
			return emitChunk(ChunkDelta{}, &stop)
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStreamAbort) {
		// Covers both emit failures (client gone: writing is pointless) and a
		// backend that returned an error without a terminal delta.
		if r.Context().Err() != nil || isWriteError(err) {
			return
		}
		streamErr = err
	}
	if streamErr != nil {
		// Headers are already sent, so report the failure in-band and stop.
		_ = writeEvent(w, sseError{Error: APIError{
			Message: redact.Error(streamErr, h.secrets...),
			Type:    errTypeFor(streamErr),
		}})
		flusher.Flush()
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flusher.Flush()
}

func (h *Handler) models(w http.ResponseWriter, r *http.Request) {
	if !allowedMethods(w, r, http.MethodGet) {
		return
	}
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, errTypeAuth, "invalid or missing API token")
		return
	}
	created := h.now().Unix()
	writeJSON(w, http.StatusOK, ModelList{
		Object: "list",
		Data: []Model{
			{ID: ModelThinkFast, Object: "model", Created: created, OwnedBy: h.backend.Name()},
			{ID: ModelThinkHard, Object: "model", Created: created, OwnedBy: h.backend.Name()},
		},
	})
}

func (h *Handler) images(w http.ResponseWriter, r *http.Request) {
	if !allowedMethods(w, r, http.MethodPost) {
		return
	}
	if !h.authorized(r) {
		writeError(w, http.StatusUnauthorized, errTypeAuth, "invalid or missing API token")
		return
	}
	writeError(w, http.StatusNotImplemented, errTypeNotImplemented,
		"image generation is not supported: the meta.ai capture exposes no image endpoint")
}

// authorized enforces the Bearer token, or allows everything when no token is
// configured.
func (h *Handler) authorized(r *http.Request) bool {
	if h.apiToken == "" {
		return true
	}
	auth := r.Header.Get("Authorization")
	scheme, token, ok := strings.Cut(auth, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(token)), []byte(h.apiToken)) == 1
}

// decodeChatRequest reads and validates the request body.
func decodeChatRequest(w http.ResponseWriter, r *http.Request) (ChatCompletionRequest, error) {
	var req ChatCompletionRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		return req, fmt.Errorf("read request body: %v", err)
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return req, fmt.Errorf("invalid JSON body: %v", err)
	}
	if len(req.Messages) == 0 {
		return req, errors.New("messages must contain at least one message")
	}
	for i, m := range req.Messages {
		if strings.TrimSpace(m.Role) == "" {
			return req, fmt.Errorf("messages[%d].role must not be empty", i)
		}
	}
	return req, nil
}

// writeJSON writes v as JSON with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	raw, err := json.Marshal(v)
	if err != nil {
		http.Error(w, `{"error":{"message":"encode response","type":"server_error"}}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(raw)
}

// writeError writes the OpenAI error envelope.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, ErrorResponse{Error: APIError{Message: msg, Type: typ}})
}

// writeEvent writes one SSE event carrying v as JSON.
func writeEvent(w io.Writer, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", raw)
	return err
}

// errTypeFor maps a backend error to the APIError.Type field.
func errTypeFor(err error) string {
	switch {
	case errors.Is(err, backend.ErrSessionExpired):
		return errTypeAuth
	case errors.Is(err, backend.ErrNotImplemented):
		return errTypeNotImplemented
	default:
		return errTypeServer
	}
}

// statusFor maps a backend error to an HTTP status. The mapping is fixed by
// the backend contract so every caller agrees.
func statusFor(err error) int {
	switch {
	case errors.Is(err, backend.ErrSessionExpired):
		return http.StatusUnauthorized
	case errors.Is(err, backend.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, backend.ErrNotImplemented):
		return http.StatusNotImplemented
	case errors.Is(err, backend.ErrUpstreamProtocol):
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

// writeBackendError reports a backend failure before any body was written.
func (h *Handler) writeBackendError(w http.ResponseWriter, err error) {
	msg := err.Error()
	if errors.Is(err, backend.ErrSessionExpired) {
		msg = "meta.ai session expired, update META_COOKIE"
	}
	// Upstream text is arbitrary and may quote a cookie fragment or token, so
	// /v1 scrubs with the same helper the admin surface uses.
	writeError(w, statusFor(err), errTypeFor(err), redact.Error(errors.New(msg), h.secrets...))
}

// randomID returns 24 hex characters of randomness for request ids.
func randomID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%024x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

var (
	// errStreamAbort stops the backend after a terminal Delta carrying Err.
	errStreamAbort = errors.New("openai: stream aborted")
)

// isWriteError reports whether err came from writing to the client, in which
// case the connection is gone and there is nothing to report.
func isWriteError(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, http.ErrHandlerTimeout) ||
		strings.Contains(err.Error(), "broken pipe") ||
		strings.Contains(err.Error(), "connection reset")
}
