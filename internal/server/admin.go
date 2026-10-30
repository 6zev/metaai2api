package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/proxy"
)

const maxConfigBody = 1 << 20

// updateStatus is the update view reported by /admin/health.
type updateStatus struct {
	Available bool   `json:"available"`
	Checked   bool   `json:"checked"`
	Current   string `json:"current,omitempty"`
	Latest    string `json:"latest,omitempty"`
	At        string `json:"at,omitempty"`
}

// updateResult is what POST /admin/update reports.
type updateResult struct {
	Applied bool   `json:"applied"`
	Version string `json:"version"`
}

// UpdateFunc performs one update check. An error is surfaced as a redacted 502.
type UpdateFunc func() (updateResult, error)

// SetUpdater installs the OTA update trigger. Nil disables POST /admin/update.
func (s *Server) SetUpdater(f UpdateFunc) {
	s.updMu.Lock()
	s.upd = f
	s.updMu.Unlock()
}

// SetUpdateChecker installs the view /admin/health reports. Nil reports
// checked=false and contacts nothing.
func (s *Server) SetUpdateChecker(f func() updateStatus) {
	s.updMu.Lock()
	s.updateChk = f
	s.updMu.Unlock()
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSONView(w, http.StatusOK, s.redactedConfig())
	case http.MethodPut:
		s.putConfig(w, r)
	default:
		methodNotAllowed(w, r, http.MethodGet, http.MethodPut)
	}
}

// putConfig is all-or-nothing: decode, validate, build the transport, then
// apply config and transport under one lock, then persist. A persistence
// failure rolls the swap back, so the previous config and transport remain in
// effect whenever the response is not 200.
func (s *Server) putConfig(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxConfigBody))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonError("read request body: "+s.redact(err.Error())))
		return
	}
	next, err := decodeConfig(body)
	if err != nil {
		// The decoder's message quotes request text, and a value the caller is
		// trying to set is not part of any configuration yet, so no scrub list
		// covers it. Classify for the client; keep the detail in the server log,
		// scrubbed, where it can be acted on.
		var de *decodeError
		reason := "decode config: the request body is not valid JSON"
		if errors.As(err, &de) {
			reason = de.Reason
		}
		// The detail stays server-side, but it may quote a value the caller was
		// trying to set, which is not yet part of any config — so it must not be
		// logged verbatim either. The decoder's message is dropped from the log
		// for that reason; the reason is enough to diagnose a bad body, and the
		// offending body is in the operator's own request log if they need it.
		s.warnLogger().Warn("config body rejected",
			slog.String("request_id", r.Header.Get("X-Request-Id")),
			slog.String("reason", reason),
			slog.Int("body_bytes", len(body)))
		writeJSON(w, http.StatusBadRequest, jsonError(reason))
		return
	}
	if strings.TrimSpace(next.AdminToken) == "" {
		// Clearing the admin token would silently dismantle the surface the
		// operator is using; require a restart with the flag removed.
		writeJSON(w, http.StatusBadRequest, jsonError("admin_token must not be empty"))
		return
	}

	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	// The listener is bound once at startup and cannot be moved without
	// restarting. Accepting a new value would report success, persist it, and
	// leave the process on the old port — so refuse it outright.
	if next.Listen != s.Config().Listen {
		writeJSON(w, http.StatusBadRequest, jsonError(
			"listen cannot be changed at runtime (current "+s.Config().Listen+"); restart with the new value"))
		return
	}

	if err := next.Validate(); err != nil {
		// Validate may quote a value from the configuration being applied, so
		// scrub with that configuration, not only the active one.
		writeJSON(w, http.StatusBadRequest, jsonError("invalid config: "+s.redactWith(next, err.Error())))
		return
	}
	b, err := backendFor(next)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonError("invalid config: "+s.redactWith(next, err.Error())))
		return
	}

	// The /v1 handler closes over the backend and the secret list, so it must be
	// rebuilt whenever either can have changed. Building it before anything is
	// published keeps the update all-or-nothing.
	prevV1 := s.v1Handler()
	nextV1, err := newV1HandlerFor(next, b)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, jsonError("build v1 handler: "+s.redactWith(next, err.Error())))
		return
	}

	prevCfg, prevBackend, prevTransport := s.state.swap(next, b, nil)
	if s.applyTr != nil {
		if err := s.applyTr(next); err != nil {
			s.restore(prevCfg, prevBackend, prevTransport)
			s.setV1Handler(prevV1)
			writeJSON(w, http.StatusInternalServerError, jsonError("build transport: "+s.redactWith(next, err.Error())))
			return
		}
		s.state.setTransport(proxy.Current())
	}

	if err := s.store.Save(next); err != nil {
		s.restore(prevCfg, prevBackend, prevTransport)
		s.setV1Handler(prevV1)
		writeJSON(w, http.StatusInternalServerError, jsonError("persist config: "+s.redactWith(next, err.Error())))
		return
	}

	s.setV1Handler(nextV1)

	// Nothing holds the previous backend or the previous transport any more:
	// requests read them through the same lock, and the handler was replaced
	// above. Release them so a PUT does not leak a connection pool or a socket.
	releaseBackend(prevBackend)
	releaseTransport(prevTransport)

	s.adminMu.Lock()
	s.adminTok = next.AdminToken
	s.adminMu.Unlock()

	writeJSONView(w, http.StatusOK, s.redactedConfig())
}

// restore puts an earlier triple back and re-publishes its transport pointer,
// so a rejected update leaves both the configuration and the exact transport
// pointer that was in effect before it started.
func (s *Server) restore(cfg config.Config, b backend.ChatBackend, tr *http.Transport) {
	s.state.swap(cfg, b, tr)
	if s.applyTr != nil {
		_ = s.applyTr(cfg)
	}
}

// releaseBackend closes a backend the server has stopped publishing. A backend
// may own a socket or a stream, so dropping the last reference without closing
// it leaks until the process exits.
func releaseBackend(b backend.ChatBackend) {
	if c, ok := b.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

// releaseTransport drops the idle connections held by a transport the server
// has stopped publishing. Without it every config change strands a pool.
func releaseTransport(t *http.Transport) {
	if t != nil {
		t.CloseIdleConnections()
	}
}

func (s *Server) adminTokenValue() string {
	s.adminMu.RLock()
	defer s.adminMu.RUnlock()
	return s.adminTok
}

// jsonConfig is the wire shape of config.Config at the admin boundary.
//
// config.Config carries only YAML tags, so encoding it directly would emit
// exported Go names; the admin API promises the config field names, which are
// snake_case everywhere else.
type jsonConfig struct {
	Listen      string      `json:"listen"`
	APIToken    string      `json:"api_token"`
	AdminToken  string      `json:"admin_token"`
	Cookie      string      `json:"cookie"`
	AccessToken string      `json:"access_token"`
	AgentType   string      `json:"agent_type"`
	ProxyURL    string      `json:"proxy_url"`
	Backend     string      `json:"backend"`
	Updater     jsonUpdater `json:"updater"`
}

type jsonUpdater struct {
	Enabled       bool   `json:"enabled"`
	Owner         string `json:"owner"`
	Repo          string `json:"repo"`
	Interval      string `json:"interval"`
	AssetPattern  string `json:"asset_pattern"`
	DGWGoldenPath string `json:"dgw_golden_path"`
}

// decodeConfig turns the snake_case wire body into config.Config, rejecting
// unknown fields so a typo cannot silently reset a setting.
func decodeConfig(body []byte) (config.Config, error) {
	var wire jsonConfig
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return config.Config{}, classifyDecodeError(err)
	}
	if dec.More() {
		return config.Config{}, errTrailingData()
	}
	cfg := config.Config{
		Listen:      wire.Listen,
		APIToken:    wire.APIToken,
		AdminToken:  wire.AdminToken,
		Cookie:      wire.Cookie,
		AccessToken: wire.AccessToken,
		AgentType:   wire.AgentType,
		ProxyURL:    wire.ProxyURL,
		Backend:     wire.Backend,
		Updater: config.Updater{
			Enabled:       wire.Updater.Enabled,
			Owner:         wire.Updater.Owner,
			Repo:          wire.Updater.Repo,
			AssetPattern:  wire.Updater.AssetPattern,
			DGWGoldenPath: wire.Updater.DGWGoldenPath,
		},
	}
	if strings.TrimSpace(wire.Updater.Interval) != "" {
		d, err := time.ParseDuration(strings.TrimSpace(wire.Updater.Interval))
		if err != nil {
			// time.ParseDuration's message quotes the caller's value.
			return config.Config{}, classifyDecodeError(fmt.Errorf("invalid duration: %w", err))
		}
		cfg.Updater.Interval = config.Duration(d)
	}
	return cfg, nil
}

// encodeConfig is the inverse of decodeConfig, used for every config response.
func encodeConfig(cfg config.Config) jsonConfig {
	interval := ""
	if cfg.Updater.Interval != 0 {
		interval = cfg.Updater.Interval.Std().String()
	}
	return jsonConfig{
		Listen:      cfg.Listen,
		APIToken:    cfg.APIToken,
		AdminToken:  cfg.AdminToken,
		Cookie:      cfg.Cookie,
		AccessToken: cfg.AccessToken,
		AgentType:   cfg.AgentType,
		ProxyURL:    cfg.ProxyURL,
		Backend:     cfg.Backend,
		Updater: jsonUpdater{
			Enabled:       cfg.Updater.Enabled,
			Owner:         cfg.Updater.Owner,
			Repo:          cfg.Updater.Repo,
			Interval:      interval,
			AssetPattern:  cfg.Updater.AssetPattern,
			DGWGoldenPath: cfg.Updater.DGWGoldenPath,
		},
	}
}

func (s *Server) redactedConfig() jsonConfig {
	red := s.Config().Redacted()
	return encodeConfig(red)
}

// redact removes every configured secret from an outbound message. Error text
// is arbitrary: a URL parser, a decoder or an upstream may quote the input.
func (s *Server) redact(msg string) string {
	return redactedMessage(errors.New(msg), s.secrets()...)
}

// TransportFor is the production transport hook for PUT /admin/config. It
// builds the new transport before publishing it, so a bad proxy URL leaves the
// current transport untouched.
func TransportFor(cfg config.Config) error {
	u, err := parseProxyURL(cfg.ProxyURL)
	if err != nil {
		return err
	}
	tr, err := proxy.Build(u)
	if err != nil {
		return err
	}
	proxy.Provide(tr)
	return nil
}

// ---- runtime state ----

// state holds the values the handler mutates. The config, the backend and the
// transport pointer move together under one lock.
type state struct {
	mu        sync.RWMutex
	cfg       config.Config
	backend   backend.ChatBackend
	transport *http.Transport
}

// swap publishes cfg, b and tr and returns the previous triple. Restoring an
// earlier triple restores the exact transport pointer: rebuilding it would
// install a different transport that describes the same proxy.
func (st *state) swap(cfg config.Config, b backend.ChatBackend, tr *http.Transport) (config.Config, backend.ChatBackend, *http.Transport) {
	st.mu.Lock()
	defer st.mu.Unlock()
	prevCfg, prevBackend, prevTransport := st.cfg, st.backend, st.transport
	st.cfg = cfg
	st.backend = b
	st.transport = tr
	return prevCfg, prevBackend, prevTransport
}

func (st *state) setTransport(t *http.Transport) {
	st.mu.Lock()
	st.transport = t
	st.mu.Unlock()
}

func (st *state) snapshotTransport() (config.Config, backend.ChatBackend, *http.Transport) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cfg, st.backend, st.transport
}

func (st *state) snapshot() (config.Config, backend.ChatBackend) {
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.cfg, st.backend
}

// errRunnerNotConfigured is the public, non-secret reason POST /admin/update
// answers 503 with when no updater was installed.
var errRunnerNotConfigured = errors.New("updater is not configured")
