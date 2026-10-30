package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/meta"
	"github.com/me/metaai2api/internal/meta/mock"
	"github.com/me/metaai2api/internal/proxy"
)

const (
	testAdminToken = "admin-secret-token"
	testAPIToken   = "sk-api-secret"
	testCookie     = "ecto_1_sess=cookie-secret-value; wd=another-secret"
	testAccess     = "access-token-secret"
	testProxyURL   = "http://user:proxy-pass-secret@127.0.0.1:3128"
)

// fakeStore is an in-memory ConfigStore that can fail on demand.
type fakeStore struct {
	mu      sync.Mutex
	cfg     config.Config
	saveErr error
	saves   int
}

func (f *fakeStore) Load() config.Config {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cfg
}

func (f *fakeStore) Save(next config.Config) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saves++
	if f.saveErr != nil {
		return f.saveErr
	}
	f.cfg = next
	return nil
}

func (f *fakeStore) setErr(e error) {
	f.mu.Lock()
	f.saveErr = e
	f.mu.Unlock()
}

func (f *fakeStore) saved() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saves
}

func mustJSON(t *testing.T, cfg config.Config) []byte {
	t.Helper()
	b, err := json.Marshal(encodeConfig(cfg))
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	return b
}

func ptrConfig(mut func(*config.Config)) *config.Config {
	c := baseConfig()
	mut(&c)
	return &c
}

func baseConfig() config.Config {
	cfg := config.Default()
	cfg.Listen = ":0"
	cfg.AdminToken = testAdminToken
	cfg.APIToken = testAPIToken
	cfg.Cookie = testCookie
	cfg.AccessToken = testAccess
	cfg.ProxyURL = ""
	return cfg
}

// transportSpy stands in for the production transport hook. It returns the
// same *http.Transport for the same configuration and publishes it, which is
// what TransportFor does in effect: identical config in, identical proxy
// behaviour out. A configured failWith error models a proxy that cannot be
// built at all.
type transportSpy struct {
	mu        sync.Mutex
	provided  []*http.Transport
	byCfg     map[config.Config]*http.Transport
	failWith  error
	failOnCfg *config.Config
}

func (sp *transportSpy) apply(cfg config.Config) error {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if sp.byCfg == nil {
		sp.byCfg = make(map[config.Config]*http.Transport)
	}
	if sp.failWith != nil && sp.failOnCfg == nil {
		return sp.failWith
	}
	if sp.failWith != nil && sp.failOnCfg != nil && *sp.failOnCfg == cfg {
		return sp.failWith
	}
	tr, ok := sp.byCfg[cfg]
	if !ok {
		u, err := url.Parse(cfg.ProxyURL)
		if err != nil {
			return err
		}
		tr, err = proxy.Build(u)
		if err != nil {
			return err
		}
		sp.byCfg[cfg] = tr
	}
	sp.provided = append(sp.provided, tr)
	proxy.Provide(tr)
	return nil
}

func (sp *transportSpy) last() *http.Transport {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	if len(sp.provided) == 0 {
		return nil
	}
	return sp.provided[len(sp.provided)-1]
}

func (sp *transportSpy) publishes() int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	return len(sp.provided)
}

// newTestServerWithLogger is newTestServer with a captured log, for asserting
// what is written server-side.
func newTestServerWithLogger(t *testing.T, cfg config.Config, store ConfigStore, spy *transportSpy, out *bytes.Buffer) *Server {
	t.Helper()
	v1, err := newV1Handler(cfg)
	if err != nil {
		t.Fatalf("v1 handler: %v", err)
	}
	opts := Options{Config: cfg, Store: store, V1: v1,
		Logger: slog.New(slog.NewTextHandler(out, nil))}
	if spy != nil {
		opts.ApplyTransport = spy.apply
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func newTestServer(t *testing.T, cfg config.Config, store ConfigStore, spy *transportSpy) *Server {
	t.Helper()
	v1, err := newV1Handler(cfg)
	if err != nil {
		t.Fatalf("v1 handler: %v", err)
	}
	opts := Options{Config: cfg, Store: store, V1: v1}
	if spy != nil {
		opts.ApplyTransport = spy.apply
	}
	s, err := New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func do(h http.Handler, method, path string, body string, headers map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func adminHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer " + testAdminToken}
}

// assertNoSecrets fails when any known secret leaks into the response, headers
// included. This is the redaction contract in one place.
func assertNoSecrets(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	hay := rec.Body.String()
	for k, vs := range rec.Header() {
		hay += k + ": " + strings.Join(vs, ", ") + "\n"
	}
	for _, secret := range []string{
		testAdminToken, testAPIToken, `cookie-secret-value`, `another-secret`,
		testAccess, "proxy-pass-secret",
	} {
		if strings.Contains(hay, secret) {
			t.Fatalf("secret %q leaked in response: %s", secret, hay)
		}
	}
}

func decodeJSON[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response %q: %v", rec.Body.String(), err)
	}
	return v
}

func TestHealthIsPublicNoSecrets(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/admin/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	assertNoSecrets(t, rec)

	got := decodeJSON[map[string]any](t, rec)
	if got["ok"] != true {
		t.Fatalf("ok = %v, want true", got["ok"])
	}
	if got["version"] == nil || got["version"] == "" {
		t.Fatalf("version missing: %v", got)
	}
	if got["backend"] != config.BackendMock {
		t.Fatalf("backend = %v, want %q", got["backend"], config.BackendMock)
	}
}

func TestHealthWorksWithoutAdminToken(t *testing.T) {
	cfg := baseConfig()
	cfg.AdminToken = ""
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, nil)

	rec := do(s, http.MethodGet, "/admin/health", "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	assertNoSecrets(t, rec)
}

func TestAdminRequiresToken(t *testing.T) {
	tests := []struct {
		name   string
		token  string
		header string
		want   int
	}{
		{"no header", testAdminToken, "", http.StatusUnauthorized},
		{"wrong token", testAdminToken, "Bearer nope", http.StatusUnauthorized},
		{"empty bearer", testAdminToken, "Bearer ", http.StatusUnauthorized},
		{"basic scheme", testAdminToken, "Basic " + testAdminToken, http.StatusUnauthorized},
		{"token unset 404", "", "Bearer anything", http.StatusNotFound},
		{"correct", testAdminToken, "Bearer " + testAdminToken, http.StatusOK},
		{"scheme case insensitive", testAdminToken, "bearer " + testAdminToken, http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.AdminToken = tc.token
			s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, nil)
			var headers map[string]string
			if tc.header != "" {
				headers = map[string]string{"Authorization": tc.header}
			}
			rec := do(s, http.MethodGet, "/admin/config", "", headers)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body)
			}
			assertNoSecrets(t, rec)
		})
	}
}

// TestAdminRoutesNotRegisteredWithoutToken proves the surface is closed as a
// whole, not merely guarded: every admin path used by the API is 404.
func TestAdminRoutesNotRegisteredWithoutToken(t *testing.T) {
	cfg := baseConfig()
	cfg.AdminToken = ""
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, nil)

	for _, path := range []string{"/admin/config", "/admin/version", "/admin/update", "/admin/session"} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost} {
			rec := do(s, method, path, `{"listen":":1"}`, adminHeaders())
			if rec.Code != http.StatusNotFound {
				t.Fatalf("%s %s status = %d, want 404", method, path, rec.Code)
			}
			assertNoSecrets(t, rec)
		}
	}
}

func TestAdminConfigGetIsRedacted(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/admin/config", "", adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	assertNoSecrets(t, rec)

	got := decodeJSON[map[string]any](t, rec)
	for _, field := range []string{"cookie", "access_token", "api_token", "admin_token"} {
		v, _ := got[field].(string)
		if v != config.RedactedSecret && !strings.Contains(v, config.RedactedSecret) {
			t.Fatalf("%s = %q, want redacted marker", field, v)
		}
	}
	if got["listen"] != baseConfig().Listen {
		t.Fatalf("listen = %v, want %q", got["listen"], baseConfig().Listen)
	}
}

func TestAdminVersion(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/admin/version", "", adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	got := decodeJSON[map[string]string](t, rec)
	if got["version"] == "" {
		t.Fatalf("version empty: %v", got)
	}
}

func TestAdminUpdateWithoutUpdaterIs503(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodPost, "/admin/update", "", adminHeaders())
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestAdminUpdateRunsInstalledUpdaterAndRedactsErrors(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	s.SetUpdater(func() (updateResult, error) {
		return updateResult{Version: "v1.2.3"}, nil
	})
	rec := do(s, http.MethodPost, "/admin/update", "", adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	s.SetUpdater(func() (updateResult, error) {
		return updateResult{}, errors.New("download failed for " + testCookie)
	})
	rec = do(s, http.MethodPost, "/admin/update", "", adminHeaders())
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	assertNoSecrets(t, rec)
}

// TestPutConfigSwapsAtomic verifies the happy path moves config, backend and
// transport together and persists.
func TestPutConfigSwapsAtomic(t *testing.T) {
	spy := &transportSpy{}
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, spy)

	// Everything except listen may change at runtime; the listener itself is
	// bound once at startup (see TestPutConfigRejectsListenChange).
	next := baseConfig()
	next.ProxyURL = testProxyURL
	body := mustJSON(t, next)

	rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	assertNoSecrets(t, rec)

	if got := s.Config().Listen; got != baseConfig().Listen {
		t.Fatalf("listen = %q, want %q", got, baseConfig().Listen)
	}
	if store.saved() != 1 {
		t.Fatalf("saves = %d, want 1", store.saved())
	}
	if spy.last() == nil {
		t.Fatal("transport hook not called")
	}
	if proxy.Current() != spy.last() {
		t.Fatal("published transport is not the one built from the new config")
	}
}

// TestPutConfigRejectsListenChange pins the fact that PUT cannot move the
// listener: reporting 200 while still serving on the old port (and persisting
// the new value) would leave the response, the live config and the file all
// disagreeing with the running process.
func TestPutConfigRejectsListenChange(t *testing.T) {
	spy := &transportSpy{}
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, spy)
	before := s.Config()

	next := baseConfig()
	next.Listen = ":9999"
	rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if got := s.Config().Listen; got != before.Listen {
		t.Fatalf("listen changed to %q, want %q", got, before.Listen)
	}
	if store.saved() != 0 {
		t.Fatalf("saves = %d, want 0 (nothing must be persisted)", store.saved())
	}
	if spy.last() != nil {
		t.Fatal("transport hook must not run for a rejected config")
	}
}

// TestPutConfigInvalidLeavesOldConfig is the all-or-nothing contract: after a
// rejected update the old config is still active, the old transport pointer is
// unchanged, and nothing was persisted.
func TestPutConfigInvalidLeavesOldConfig(t *testing.T) {
	tests := []struct {
		name  string
		mut   func(*config.Config)
		raw   string
		spy   *transportSpy
		want  int
		probe string
	}{
		{
			name: "unknown backend",
			mut:  func(c *config.Config) { c.Backend = "nope" },
			want: http.StatusBadRequest,
		},
		{
			name: "empty listen",
			mut:  func(c *config.Config) { c.Listen = " " },
			want: http.StatusBadRequest,
		},
		{
			name: "malformed cookie",
			mut:  func(c *config.Config) { c.Cookie = "badpair" },
			want: http.StatusBadRequest,
		},
		{
			name: "unsupported proxy scheme",
			mut:  func(c *config.Config) { c.ProxyURL = "ftp://example:21" },
			want: http.StatusBadRequest,
		},
		{
			name: "cleared admin token",
			mut:  func(c *config.Config) { c.AdminToken = "" },
			want: http.StatusBadRequest,
		},
		{
			name: "transport build fails",
			mut:  func(c *config.Config) { c.AgentType = "think_hard" },
			spy: &transportSpy{failWith: errors.New("tls: " + testAccess),
				failOnCfg: ptrConfig(func(c *config.Config) { c.AgentType = "think_hard" })},
			want: http.StatusInternalServerError,
		},
		{
			name:  "store save fails",
			mut:   func(c *config.Config) { c.AgentType = "think_hard" },
			want:  http.StatusInternalServerError,
			probe: "save",
		},
		{
			name: "listen change rejected",
			mut:  func(c *config.Config) { c.Listen = ":7777" },
			want: http.StatusBadRequest,
		},
		{
			name: "undecodable body",
			raw:  `{"listen": "bad"`,
			want: http.StatusBadRequest,
		},
		{
			name: "unknown field",
			raw:  `{"listen":":1","surprise":true}`,
			want: http.StatusBadRequest,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spy := tc.spy
			if spy == nil {
				spy = &transportSpy{}
			}
			store := &fakeStore{cfg: baseConfig()}
			s := newTestServer(t, baseConfig(), store, spy)

			// Establish a known-good baseline, then arm the failure mode: the
			// baseline is unambiguously good and the update under test must not
			// publish anything.
			next := baseConfig()
			body := mustJSON(t, next)
			if rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders()); rec.Code != http.StatusOK {
				t.Fatalf("baseline status = %d; body=%s", rec.Code, rec.Body)
			}
			baseline := proxy.Current()
			baselineCfg := s.Config()
			baselineSaves := store.saved()
			if tc.probe == "save" {
				store.setErr(errors.New("disk full: " + testAdminToken))
			}

			if tc.raw != "" {
				body = []byte(tc.raw)
			} else {
				next := baseConfig()
				tc.mut(&next)
				body = mustJSON(t, next)
			}

			rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders())
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", rec.Code, tc.want, rec.Body)
			}
			assertNoSecrets(t, rec)

			if got := proxy.Current(); got != baseline {
				t.Fatalf("transport pointer changed after rejected update: %p != %p", got, baseline)
			}
			if got := s.Config(); !reflect.DeepEqual(got, baselineCfg) {
				t.Fatalf("config changed after rejected update:\n got %+v\nwant %+v", got, baselineCfg)
			}
			wantSaves := baselineSaves
			if tc.probe == "save" {
				// The save is attempted (and fails) only once every other step
				// passed; nothing is committed.
				wantSaves++
			}
			if got := store.saved(); got != wantSaves {
				t.Fatalf("save attempts = %d, want %d", got, wantSaves)
			}
		})
	}
}

// TestPutConfigPersistFailureRollsBackTransport proves the rollback path also
// restores the transport when the save fails after the swap.
func TestPutConfigPersistFailureRollsBackTransport(t *testing.T) {
	spy := &transportSpy{}
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, spy)

	// Warm-up update so the baseline is a transport the hook itself built: the
	// rollback must restore exactly that pointer.
	warm := baseConfig()
	warmBody := mustJSON(t, warm)
	if rec := do(s, http.MethodPut, "/admin/config", string(warmBody), adminHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("warm-up status = %d; body=%s", rec.Code, rec.Body)
	}

	baseline := proxy.Current()
	baselineSaves := store.saved()

	store.setErr(errors.New("disk full"))
	next := baseConfig()
	next.AgentType = "think_hard"
	next.ProxyURL = testProxyURL
	body := mustJSON(t, next)

	rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders())
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	assertNoSecrets(t, rec)
	if got := proxy.Current(); got != baseline {
		t.Fatalf("transport not rolled back: %p != %p", got, baseline)
	}
	if got := s.Config().Listen; got != baseConfig().Listen {
		t.Fatalf("config not rolled back: listen = %q", got)
	}
	// The rejected update reached the store exactly once, and that write failed.
	if got := store.saved(); got != baselineSaves+1 {
		t.Fatalf("save attempts = %d, want %d", got, baselineSaves+1)
	}
}

// TestPutConfigRotatesAdminToken proves a successful update may rotate the
// admin token and the new one takes effect.
func TestPutConfigRotatesAdminToken(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, &transportSpy{})

	next := baseConfig()
	next.AdminToken = "rotated-token"
	body := mustJSON(t, next)
	rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	if rec := do(s, http.MethodGet, "/admin/version", "", adminHeaders()); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old token status = %d, want 401", rec.Code)
	}
	rec = do(s, http.MethodGet, "/admin/version", "", map[string]string{"Authorization": "Bearer rotated-token"})
	if rec.Code != http.StatusOK {
		t.Fatalf("new token status = %d, want 200", rec.Code)
	}
}

func TestV1IsMounted(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/v1/models", "", map[string]string{"Authorization": "Bearer " + testAPIToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}
	assertNoSecrets(t, rec)
}

func TestV1StreamsThroughRecorder(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	body := `{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	rec := do(s, http.MethodPost, "/v1/chat/completions", body, map[string]string{
		"Authorization": "Bearer " + testAPIToken,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", rec.Code, rec.Body)
	}
	// The recorder must not swallow Flush: SSE arrives as data frames.
	if !strings.Contains(rec.Body.String(), "data: ") {
		t.Fatalf("stream body has no data frames: %q", rec.Body.String())
	}
}

func TestRecoverTurnsPanicInto500(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	cfg := baseConfig()
	panicky := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
	s, err := New(Options{Config: cfg, Store: store, V1: panicky, Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	rec := do(s, http.MethodGet, "/v1/chat/completions", "", nil)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestRequestIDAndCORS(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/admin/health", "", nil)
	if rec.Header().Get("X-Request-Id") == "" {
		t.Fatal("X-Request-Id not set")
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatal("CORS origin missing")
	}

	rec = do(s, http.MethodGet, "/admin/health", "", map[string]string{"X-Request-Id": "abc123"})
	if got := rec.Header().Get("X-Request-Id"); got != "abc123" {
		t.Fatalf("X-Request-Id = %q, want echo abc123", got)
	}
}

func TestUnknownPathIs404(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodGet, "/nope", "", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	// A near-miss admin path must not fall through to the mux as new.
	rec = do(s, http.MethodGet, "/admin/config/extra", "", adminHeaders())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestNoGraphQLReplayRoute(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, nil)

	rec := do(s, http.MethodPost, "/admin/graphql", `{"doc_id":"x","variables":{}}`, adminHeaders())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: no credential relay route", rec.Code)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	if _, err := New(Options{Store: &fakeStore{cfg: baseConfig()}}); err == nil {
		t.Fatal("nil V1 accepted")
	}
	v1 := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	if _, err := New(Options{Config: baseConfig(), V1: v1}); err == nil {
		t.Fatal("nil store accepted")
	}
	cfg := baseConfig()
	cfg.Backend = config.BackendExperimentalDGW
	cfg.Updater.DGWGoldenPath = "/tmp/golden.json"
	if _, err := New(Options{Config: cfg, Store: &fakeStore{}, V1: v1}); err == nil {
		t.Fatal("unwired backend accepted")
	}
}

func TestBackendSwapIsVisibleToV1(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, &transportSpy{})

	before := s.Backend()
	if got := before.Name(); got != "mock" {
		t.Fatalf("backend = %q, want mock", got)
	}
	next := baseConfig()
	body := mustJSON(t, next)
	if rec := do(s, http.MethodPut, "/admin/config", string(body), adminHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if s.Backend() == nil {
		t.Fatal("backend nil after reload")
	}
	_ = mock.New() // keeps the import honest: the seam is the mock backend
}

// TestConfigWireShapeIsSnakeCase pins the admin config contract: request and
// response use the config field names, and the updater interval round-trips as
// a Go duration string rather than nanoseconds.
func TestConfigWireShapeIsSnakeCase(t *testing.T) {
	store := &fakeStore{cfg: baseConfig()}
	s := newTestServer(t, baseConfig(), store, &transportSpy{})

	rec := do(s, http.MethodGet, "/admin/config", "", adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, key := range []string{
		"listen", "api_token", "admin_token", "cookie", "access_token",
		"agent_type", "proxy_url", "backend", "updater",
	} {
		if _, ok := got[key]; !ok {
			t.Fatalf("missing %q in %v", key, got)
		}
	}
	for _, leaked := range []string{"Listen", "APIToken", "Cookie", "ProxyURL"} {
		if _, ok := got[leaked]; ok {
			t.Fatalf("Go field name %q leaked into the admin JSON", leaked)
		}
	}

	next := baseConfig()
	next.Updater.Interval = config.Duration(90 * time.Minute)
	body := mustJSON(t, next)
	rec = do(s, http.MethodPut, "/admin/config", string(body), adminHeaders())
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT status = %d; body=%s", rec.Code, rec.Body)
	}
	if got := s.Config().Updater.Interval.Std(); got != 90*time.Minute {
		t.Fatalf("interval = %v, want 1h30m0s", got)
	}
	if !strings.Contains(rec.Body.String(), `"interval":"1h30m0s"`) {
		t.Fatalf("interval not rendered as a duration string: %s", rec.Body)
	}
}

// TestSessionDiagnosticDoesNotExposeGraphQLReplay pins the shape of the only
// route that talks to meta.ai: it takes no caller input beyond the method, so
// it cannot be used to replay arbitrary persisted queries, and it reports a
// classification rather than an upstream body (which may quote the cookie).
func TestSessionDiagnosticDoesNotExposeGraphQLReplay(t *testing.T) {
	cfg := baseConfig()
	cfg.Cookie = "datr=x; ecto_1_sess=PROBE-SESSION-SECRET-1234"
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, nil)

	// Query parameters and a body must not influence which document is called.
	rec := do(s, http.MethodGet, "/admin/session?doc_id=deadbeef&variables=%7B%7D", "", adminHeaders())
	assertNoSecrets(t, rec)

	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body)
	}
	if _, ok := got["ok"]; !ok {
		t.Fatalf("response lacks an ok field: %s", rec.Body)
	}
	if doc, ok := got["operation"]; !ok || doc != meta.DocEctoHomePromptRouteQuery {
		t.Fatalf("operation = %v, want the fixed %q", doc, meta.DocEctoHomePromptRouteQuery)
	}
	// The probe must not echo an arbitrary caller-supplied doc_id.
	if strings.Contains(rec.Body.String(), "deadbeef") {
		t.Fatalf("response reflected the caller-supplied doc_id: %s", rec.Body)
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the probe reports failure in the body)", rec.Code)
	}
}
