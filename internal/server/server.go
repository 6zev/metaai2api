// Package server wires the HTTP surface: middleware, the public admin health
// probe, the token-locked admin API and the mounted /v1 handler.
//
// The admin surface is deliberately narrow: no upstream replay endpoint, no
// request-body echo, and no secret ever leaves the process. When admin_token is
// empty the admin routes are not registered at all, so a misconfigured
// deployment is closed by default.
package server

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	backend "github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/proxy"
	"github.com/me/metaai2api/internal/redact"
	"github.com/me/metaai2api/internal/version"
)

// ConfigStore loads and persists the service configuration.
//
// It is a seam so the server can be tested without touching disk and so the
// persistence order (validate, swap, save, undo on failure) lives in one place.
type ConfigStore interface {
	// Load returns the active configuration.
	Load() config.Config
	// Save persists next. It is called only after next is in effect and must
	// not mutate the active configuration itself.
	Save(next config.Config) error
}

// SaveFunc adapts a persistence function to ConfigStore.
type SaveFunc func(config.Config) error

func (f SaveFunc) Save(next config.Config) error { return f(next) }

// Options configures New.
type Options struct {
	// Config is the initial configuration. An empty admin_token disables the
	// admin routes.
	Config config.Config
	// Store persists updates from PUT /admin/config. Required.
	Store ConfigStore
	// V1 is the handler mounted for every /v1 path. Required.
	V1 http.Handler
	// Logger receives request and panic logs. Nil disables logging.
	Logger *slog.Logger
	// Now overrides the clock used for request timing. Optional.
	Now func() time.Time
	// ApplyTransport publishes the transport for cfg. Nil uses TransportFor.
	// It is called before the swap and again to roll back, so it must be
	// idempotent and must not fail for a configuration that already ran.
	ApplyTransport func(config.Config) error
}

// Server is the root http.Handler. Build one with New.
type Server struct {
	store   ConfigStore
	v1      http.Handler
	logger  *slog.Logger
	now     func() time.Time
	applyTr func(config.Config) error

	state *state

	adminMux  *http.ServeMux
	adminMu   sync.RWMutex
	adminTok  string
	applyMu   sync.Mutex
	updMu     sync.Mutex
	upd       UpdateFunc
	updateChk func() updateStatus
}

// New validates opts and returns a Server.
func New(opts Options) (*Server, error) {
	if opts.Store == nil {
		return nil, errors.New("server: store is required")
	}
	if opts.V1 == nil {
		return nil, errors.New("server: v1 handler is required")
	}
	cfg := opts.Config
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	b, err := backendFor(cfg)
	if err != nil {
		return nil, err
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	apply := opts.ApplyTransport
	if apply == nil {
		apply = TransportFor
	}
	s := &Server{
		store:   opts.Store,
		v1:      opts.V1,
		logger:  opts.Logger,
		now:     now,
		applyTr: apply,
		// Record the transport that was actually published during startup so a
		// later config change knows which pool it is replacing. Without this the
		// field stays nil and nothing is ever released.
		state: &state{cfg: cfg, backend: b, transport: proxy.Current()},
	}
	if strings.TrimSpace(cfg.AdminToken) != "" {
		s.adminTok = cfg.AdminToken
		s.mountAdmin()
	}
	return s, nil
}

// backendFor resolves the backend named by cfg. A backend that is not wired
// here is a loud error, never a silent fallback to mock.
func backendFor(cfg config.Config) (backend.ChatBackend, error) {
	switch cfg.Backend {
	case config.BackendMock:
		return newMockBackend(), nil
	case config.BackendExperimentalDGW:
		// The DGW backend needs an access token, a recorded golden vector and
		// the WASM codec, none of which can be created at runtime here. Say so
		// in operator terms instead of surfacing an internal "not wired".
		return nil, fmt.Errorf(
			"backend %q must be selected at startup: it requires %s, updater.dgw_golden_path and a wasm_codec build",
			cfg.Backend, "access_token")
	default:
		return nil, fmt.Errorf("unknown backend %q: use %q or %q",
			cfg.Backend, config.BackendMock, config.BackendExperimentalDGW)
	}
}

// Config returns the active configuration, secrets included. Callers must use
// Redacted before serving it.
func (s *Server) Config() config.Config {
	cfg, _ := s.state.snapshot()
	return cfg
}

// Backend returns the backend serving the /v1 routes.
func (s *Server) Backend() backend.ChatBackend {
	_, b := s.state.snapshot()
	return b
}

// Close releases the active backend's resources when it owns any.
func (s *Server) Close() error {
	if c, ok := s.Backend().(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ServeHTTP runs the middleware chain, then routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := s.now()
	reqID := r.Header.Get("X-Request-Id")
	if reqID == "" {
		reqID = newRequestID()
	}

	rec := &recorder{w: w, status: http.StatusOK}
	rec.Header().Set("X-Request-Id", reqID)
	setCORS(rec, r)

	func() {
		defer func() {
			if v := recover(); v != nil {
				s.logRecovered(reqID, v)
				if !rec.wroteHeader {
					writeJSON(rec, http.StatusInternalServerError, jsonError("internal server error"))
				}
				// Headers are already out on a stream; the client sees a cut.
			}
		}()
		s.route(rec, r)
	}()

	s.logRequest(reqID, r, rec.status, s.now().Sub(start))
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/admin/health":
		s.handleHealth(w, r)
	case strings.HasPrefix(r.URL.Path, "/admin/"):
		s.serveAdmin(w, r)
	case strings.HasPrefix(r.URL.Path, "/v1/") || r.URL.Path == "/v1":
		s.v1Handler().ServeHTTP(w, r)
	default:
		writeJSON(w, http.StatusNotFound, jsonError("not found"))
	}
}

// v1Handler returns the currently mounted /v1 handler. It is read through the
// same lock as the rest of the state because PUT /admin/config replaces it: the
// handler closes over the secret list and the backend, so a stale copy would
// scrub with the previous cookie and serve from the previous backend.
func (s *Server) v1Handler() http.Handler {
	s.state.mu.RLock()
	defer s.state.mu.RUnlock()
	return s.v1
}

func (s *Server) setV1Handler(h http.Handler) {
	s.state.mu.Lock()
	s.v1 = h
	s.state.mu.Unlock()
}

// mountAdmin registers the token-locked routes. It runs only when a token is
// configured; without one every /admin path except health stays 404.
func (s *Server) mountAdmin() {
	m := http.NewServeMux()
	m.HandleFunc("/admin/config", s.handleConfig)
	m.HandleFunc("/admin/update", s.handleUpdate)
	m.HandleFunc("/admin/version", s.handleVersion)
	// A read-only upstream diagnostic: proves the configured cookie still works.
	// Registered here, like every other admin route, so it is 404 without a
	// token rather than silently public.
	m.HandleFunc("/admin/session", s.handleSession)
	s.adminMux = m
}

// serveAdmin checks the bearer token, then dispatches.
func (s *Server) serveAdmin(w http.ResponseWriter, r *http.Request) {
	mux := s.adminMux
	if mux == nil {
		writeJSON(w, http.StatusNotFound, jsonError("not found"))
		return
	}
	if !authorized(r, s.adminTokenValue()) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="admin"`)
		writeJSON(w, http.StatusUnauthorized, jsonError("missing or invalid admin token"))
		return
	}
	h, pattern := mux.Handler(r)
	if pattern == "" {
		writeJSON(w, http.StatusNotFound, jsonError("not found"))
		return
	}
	h.ServeHTTP(w, r)
}

// authorized compares the bearer token in constant time.
func authorized(r *http.Request, token string) bool {
	if token == "" {
		return false
	}
	h := r.Header.Get("Authorization")
	const prefix = "bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, healthResponse{
		OK:      true,
		Version: version.Version,
		Backend: s.Config().Backend,
		Update:  s.updateStatus(),
	})
}

// healthResponse is public and must never grow a secret field.
type healthResponse struct {
	OK      bool         `json:"ok"`
	Version string       `json:"version"`
	Backend string       `json:"backend"`
	Update  updateStatus `json:"update"`
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, r, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"version": version.Version,
		"backend": s.Config().Backend,
	})
}

func (s *Server) handleUpdate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, r, http.MethodPost)
		return
	}

	s.updMu.Lock()
	upd := s.upd
	s.updMu.Unlock()

	if upd == nil {
		writeJSON(w, http.StatusServiceUnavailable, jsonError(errRunnerNotConfigured.Error()))
		return
	}
	res, err := upd()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, jsonError(s.redact(err.Error())))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "update": res})
}

func (s *Server) updateStatus() updateStatus {
	s.updMu.Lock()
	chk := s.updateChk
	s.updMu.Unlock()

	if chk == nil {
		return updateStatus{Checked: false}
	}
	return chk()
}

// Secrets returns every literal string that must never appear in an outbound
// message, derived from one configuration value.
//
// It exists so there is exactly ONE place that decides what counts as a secret.
// The previous hand-written list omitted ProxyURL, which meant the field that
// Config.Redacted() was fixed to protect still reached clients whenever a dial
// error quoted the proxy. Deriving the list here means a new secret field is
// added to Redacted and to this list in the same edit, or the drift test fails.
func Secrets(cfg config.Config) []string {
	redacted := cfg.Redacted()
	out := make([]string, 0, 6)
	add := func(v string) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	add(cfg.Cookie)
	add(cfg.AccessToken)
	add(cfg.APIToken)
	add(cfg.AdminToken)
	// The raw proxy URL carries credentials verbatim...
	add(cfg.ProxyURL)
	// ...and so may its redacted form, if the password was percent-encoded and
	// the error text quotes the encoded spelling rather than the decoded one.
	add(redacted.ProxyURL)
	// A dial error may quote the DECODED password, because url.Parse hands it
	// back decoded. Add both spellings so either form is scrubbed.
	if u, err := url.Parse(strings.TrimSpace(cfg.ProxyURL)); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok && pw != "" {
			add(pw)
			// Also cover user:password, which is how a URL prints it.
			add(u.User.Username() + ":" + pw)
		}
	}
	return out
}

func (s *Server) secrets() []string {
	return Secrets(s.Config())
}

// redactWith scrubs text produced while applying next. The active config alone
// is not enough: a rejected update may quote a value that exists only in the
// configuration being applied, and that value must not reach the client just
// because it was not yet live.
func (s *Server) redactWith(next config.Config, msg string) string {
	return redact.Secrets(msg, append(Secrets(s.Config()), Secrets(next)...)...)
}

// redactedMessage returns err's text with every secret replaced. Error text is
// arbitrary: a URL parser, a JSON decoder or an upstream may quote the input.
// It delegates to internal/redact so the /v1 surface scrubs identically.
func redactedMessage(err error, secrets ...string) string {
	return redact.Error(err, secrets...)
}

// ---- middleware helpers ----

// recorder tracks the status written and whether anything was written, so the
// recover middleware knows if it can still send a clean 500.
//
// It holds the underlying writer in a private field rather than embedding it:
// an embedded http.ResponseWriter would promote its Write and WriteHeader, so
// calls would bypass the tracking entirely.
type recorder struct {
	w           http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *recorder) Header() http.Header { return r.w.Header() }

func (r *recorder) WriteHeader(code int) {
	if r.wroteHeader {
		return
	}
	r.status = code
	r.wroteHeader = true
	r.w.WriteHeader(code)
}

func (r *recorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	return r.w.Write(b)
}

// Flush keeps the SSE fast path working without a ResponseController round-trip.
func (r *recorder) Flush() {
	if f, ok := r.w.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (r *recorder) Unwrap() http.ResponseWriter { return r.w }

func setCORS(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Request-Id")
	h.Set("Access-Control-Allow-Methods", "GET, POST, PUT, OPTIONS")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Max-Age", "600")
	}
}

// warnLogger returns the configured logger, or a discard logger when none was
// supplied, so optional logging never dereferences nil.
func (s *Server) warnLogger() *slog.Logger {
	if s.logger != nil {
		return s.logger
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Server) logRequest(reqID string, r *http.Request, status int, d time.Duration) {
	if s.logger == nil {
		return
	}
	s.logger.InfoContext(r.Context(), "http request",
		slog.String("request_id", reqID),
		slog.String("method", r.Method),
		// Path only: a query string could carry a stray credential.
		slog.String("path", r.URL.Path),
		slog.Int("status", status),
		slog.Duration("duration", d),
	)
}

func (s *Server) logRecovered(reqID string, v any) {
	if s.logger == nil {
		return
	}
	s.logger.Error("panic recovered",
		slog.String("request_id", reqID),
		slog.Any("panic", v),
		slog.String("stack", string(debug.Stack())),
	)
}

func newRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// ---- helpers ----

// writeJSON writes the canonical JSON view.
func writeJSON(w http.ResponseWriter, status int, v any) {
	writeJSONView(w, status, toJSONView(v))
}

// writeJSONView writes v as-is. Use it for the config endpoints, whose field
// names must stay the config tags.
func writeJSONView(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// toJSONView maps a value through its JSON encoding so a struct that carries
// YAML tags still serializes. Config in particular would otherwise fall back to
// exported Go names at the admin boundary.
func toJSONView(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return v
	}
	return out
}

type errorBody struct {
	Error string `json:"error"`
	Type  string `json:"type"`
}

func jsonError(msg string) errorBody {
	return errorBody{Error: msg, Type: "error"}
}

func methodNotAllowed(w http.ResponseWriter, r *http.Request, want ...string) {
	w.Header().Set("Allow", strings.Join(want, ", "))
	writeJSON(w, http.StatusMethodNotAllowed, jsonError("method "+r.Method+" not allowed"))
}

// parseProxyURL parses an operator-supplied proxy URL for transport building.
func parseProxyURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy_url: %w", err)
	}
	return u, nil
}
