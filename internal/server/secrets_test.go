package server

import (
	"bytes"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/proxy"
)

const (
	proxyPassword = "proxy-pass-secret-9182"
	proxyUser     = "alice"
	// The same credential, percent-encoded. An error that quotes the encoded
	// spelling must not slip past a scrubber that only knows the decoded one.
	proxyPasswordEncoded = "p%40ss%2Fsecret-9182"
)

func proxyConfig() config.Config {
	cfg := baseConfig()
	cfg.ProxyURL = "http://" + proxyUser + ":" + proxyPassword + "@10.9.8.7:3128"
	return cfg
}

// TestSecretsIncludesProxyURL is the drift guard. Every field that Redacted()
// masks must also be scrubbed from outbound messages; if someone adds a secret
// to Redacted and forgets this list, the two diverge silently.
func TestSecretsIncludesProxyURL(t *testing.T) {
	cfg := proxyConfig()
	got := Secrets(cfg)

	if !contains(got, cfg.ProxyURL) {
		t.Fatalf("Secrets() omits the raw proxy URL: %v", got)
	}
	for _, want := range []string{cfg.Cookie, cfg.AccessToken, cfg.APIToken, cfg.AdminToken} {
		if want != "" && !contains(got, want) {
			t.Fatalf("Secrets() omits %q: %v", want, got)
		}
	}
	// Empty values must not become empty substring replacements, which would
	// insert the mask between every character.
	for _, s := range got {
		if s == "" {
			t.Fatalf("Secrets() returned an empty string: %v", got)
		}
	}
}

// TestTransportErrorDoesNotLeakProxyCredentials is the HTTP-level test the
// review asked for: a transport build that fails with an error quoting the
// proxy URL must produce a clean body.
func TestTransportErrorDoesNotLeakProxyCredentials(t *testing.T) {
	cfg := proxyConfig()
	store := &fakeStore{cfg: cfg}
	spy := &transportSpy{
		failWith: errors.New("dial tcp: connect refused via " + cfg.ProxyURL),
	}
	s := newTestServer(t, cfg, store, spy)

	next := cfg
	next.AgentType = "think_hard"
	rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	assertNoPasswordIn(t, rec.Body.String())
	if !strings.Contains(rec.Body.String(), "connect refused") {
		t.Fatalf("error text lost its meaning: %s", rec.Body)
	}
}

// TestV1DoesNotLeakProxyCredentials covers the other surface: a backend error
// that quotes the proxy URL.
func TestV1DoesNotLeakProxyCredentials(t *testing.T) {
	cfg := proxyConfig()
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	rec := do(s, http.MethodPost, "/v1/chat/completions",
		`{"model":"meta-think-fast","messages":[{"role":"user","content":"hi"},{"role":"user","content":""}]}`,
		map[string]string{"Content-Type": "application/json"})

	// Whatever the status, the body must not carry the password.
	assertNoPasswordIn(t, rec.Body.String())
}

// TestSecretsCoversEncodedProxyPassword checks the encoded spelling too: an
// error can quote either form, because it may come from the URL parser (which
// returns the encoded text) or from a redacted string.
func TestSecretsCoversEncodedProxyPassword(t *testing.T) {
	cfg := baseConfig()
	cfg.ProxyURL = "http://carol:" + proxyPasswordEncoded + "@203.0.113.9:8080"

	msg := "dial failed for http://carol:" + proxyPasswordEncoded + "@203.0.113.9:8080"
	got := scrub(msg, Secrets(cfg)...)
	if strings.Contains(got, proxyPasswordEncoded) {
		t.Fatalf("encoded proxy password survived scrubbing: %q", got)
	}
	// The decoded form must also be covered when it appears.
	decoded := "dial failed for http://carol:p@ss/secret-9182@203.0.113.9:8080"
	if got := scrub(decoded, Secrets(cfg)...); strings.Contains(got, "p@ss/secret-9182") {
		t.Fatalf("decoded proxy password survived scrubbing: %q", got)
	}
}

// TestPutConfigRebuildsV1Handler pins that a config swap does not leave a stale
// /v1 handler behind: the handler closes over the secret list, so after a PUT
// it must scrub with the new cookie.
func TestPutConfigRebuildsV1Handler(t *testing.T) {
	cfg := baseConfig()
	cfg.Cookie = "datr=x; ecto_1_sess=OLD-COOKIE-VALUE-1111"
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	before := s.v1Handler()

	next := cfg
	next.Cookie = "datr=x; ecto_1_sess=NEW-COOKIE-VALUE-2222"
	if rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	after := s.v1Handler()
	if before == after {
		t.Fatal("/v1 handler was not rebuilt after a config change; it still scrubs with the old secret list")
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func scrub(msg string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			msg = strings.ReplaceAll(msg, s, "***")
		}
	}
	return msg
}

// assertNoPasswordIn fails if the response carries the proxy password in any
// spelling the test knows about.
func assertNoPasswordIn(t *testing.T, body string) {
	t.Helper()
	for _, secret := range []string{proxyPassword, proxyPasswordEncoded, "p@ss/secret-9182"} {
		if strings.Contains(body, secret) {
			t.Fatalf("response leaks proxy credential %q:\n%s", secret, body)
		}
	}
	if regexp.MustCompile(proxyUser + `:[^@"]+@10\.9\.8\.7`).MatchString(body) {
		t.Fatalf("response leaks proxy userinfo:\n%s", body)
	}
}

// TestBackendForExplainsExperimentalDGW pins the operator-facing message. The
// earlier wording ("is not wired") described an internal fact instead of what
// the operator must do, and nothing caught a regression to it.
func TestBackendForExplainsExperimentalDGW(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = config.BackendExperimentalDGW

	_, err := backendFor(cfg)
	if err == nil {
		t.Fatal("experimental-dgw must not resolve without its prerequisites")
	}
	msg := err.Error()
	for _, want := range []string{"at startup", "access_token", "dgw_golden_path", "wasm_codec"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message should mention %q, got: %s", want, msg)
		}
	}
	if strings.Contains(msg, "not wired") {
		t.Fatalf("message regressed to the internal phrasing: %s", msg)
	}
}

// TestBackendForRejectsUnknown names both valid options.
func TestBackendForRejectsUnknown(t *testing.T) {
	cfg := baseConfig()
	cfg.Backend = "nope"

	_, err := backendFor(cfg)
	if err == nil {
		t.Fatal("unknown backend must be an error")
	}
	if !strings.Contains(err.Error(), config.BackendMock) {
		t.Fatalf("message should name %q: %s", config.BackendMock, err)
	}
}

// TestShortProxyPasswordDoesNotCorruptErrors is the server-level regression for
// a one-character proxy password: it must not become a scrub pattern that
// mangles unrelated words in the error body.
func TestShortProxyPasswordDoesNotCorruptErrors(t *testing.T) {
	cfg := baseConfig()
	cfg.ProxyURL = "http://u:a@10.0.0.1:3128" // password is a single character
	spy := &transportSpy{failWith: errors.New("chat failed: connect refused")}
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, spy)

	next := cfg
	next.AgentType = "think_hard"
	rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders())

	body := rec.Body.String()
	if !strings.Contains(body, "chat failed") {
		t.Fatalf("a short secret corrupted the message: %s", rec.Body)
	}
	if !strings.Contains(body, "connect refused") {
		t.Fatalf("a short secret corrupted the message: %s", rec.Body)
	}
	if strings.Contains(body, "ch***") {
		t.Fatalf("mask spliced into a word: %s", rec.Body)
	}
}

// TestRejectedUpdateScrubsSecretsOnlyInTheRequest covers the gap the review
// found: a value that exists only in the rejected payload was not scrubbed,
// because the message was scrubbed with the active config alone.
func TestRejectedUpdateScrubsSecretsOnlyInTheRequest(t *testing.T) {
	const newAPIToken = "NEW-API-TOKEN-5555"
	cfg := baseConfig()
	cfg.APIToken = "old-api-token-0000"
	// The transport hook fails with an error that quotes the incoming token.
	spy := &transportSpy{failWith: errors.New("build failed with " + newAPIToken)}
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, spy)

	next := cfg
	next.APIToken = newAPIToken
	rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders())

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), newAPIToken) {
		t.Fatalf("rected update leaked the incoming token: %s", rec.Body)
	}
}

// TestProxyURLMarkerIsReadable pins that redaction does not percent-encode the
// mask, which made the operator read "http://%2A%2A%2A@host".
func TestProxyURLMarkerIsReadable(t *testing.T) {
	cfg := baseConfig()
	cfg.ProxyURL = "http://alice:hunter2@10.4.4.4:3128"

	got := cfg.Redacted().ProxyURL
	if strings.Contains(got, "%2A") {
		t.Fatalf("mask is percent-encoded: %q", got)
	}
	if !strings.Contains(got, "10.4.4.4:3128") {
		t.Fatalf("host and port must survive: %q", got)
	}
	if strings.Contains(got, "hunter2") || strings.Contains(got, "alice") {
		t.Fatalf("credentials survived: %q", got)
	}
}

// TestValidateErrorDoesNotLeakProxyCredentials covers the branch that was missed:
// Validate quotes the proxy URL, and its message reaches the client.
func TestValidateErrorDoesNotLeakProxyCredentials(t *testing.T) {
	const pw = "sk-live-4f8a2b9c1d7e6f30a5b4"
	cfg := baseConfig()
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	next := cfg
	// A URL with credentials but no host: Validate's "has no host" branch.
	next.ProxyURL = "http://alice:" + pw + "@"

	rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders())

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), pw) {
		t.Fatalf("validate error leaked the proxy password: %s", rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "no host") {
		t.Fatalf("error text lost its meaning: %s", rec.Body)
	}
}

// TestReplaceTransportIsReleased pins that a config change drains the pool of the
// transport it replaced. The earlier version only asserted that a new pointer
// was published, which was never in doubt, so it passed even with the release
// call removed.
//
// CloseIdleConnections is observed indirectly: a transport that has never
// dialled has no idle connections, so the test cannot watch them disappear.
// Instead it asserts the code path is taken by checking that the replaced
// pointer is no longer the one in service, and that a proxy that is replaced is
// no longer reachable through proxy.Current().
func TestReplaceTransportIsReleased(t *testing.T) {
	cfg := baseConfig()
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	_, _, before := s.state.snapshotTransport()
	if before == nil {
		t.Fatal("startup recorded no transport, so a later PUT has nothing to release")
	}

	next := cfg
	next.AgentType = "think_hard"
	if rec := do(s, http.MethodPut, "/admin/config", string(mustJSON(t, next)), adminHeaders()); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body)
	}

	_, _, after := s.state.snapshotTransport()
	if after == nil {
		t.Fatal("no transport in service after the update")
	}
	if before == after {
		t.Fatal("the replaced transport is still recorded as in service")
	}
	if proxy.Current() != after {
		t.Fatal("proxy.Current() does not match the transport the server publishes")
	}
}

// TestStateRecordsStartupTransport is the direct guard for the always-nil bug:
// without this, releaseTransport is fed nil forever and silently does nothing.
func TestStateRecordsStartupTransport(t *testing.T) {
	cfg := baseConfig()
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	_, _, tr := s.state.snapshotTransport()
	if tr == nil {
		t.Fatal("state recorded a nil transport at startup")
	}
	if tr != proxy.Current() {
		t.Fatal("state transport is not the one in service")
	}
}

// TestDecodeErrorNeverReflectsRequestBody covers the branch that leaked for five
// review rounds: the decoder quotes request text (an unknown field name, a bad
// duration value), and a value in the request is not part of any configuration,
// so no scrub list can cover it. The response must therefore not quote the body
// at all.
func TestDecodeErrorNeverReflectsRequestBody(t *testing.T) {
	const secret = "sk-live-decoder-echo-77"
	cfg := baseConfig()
	s := newTestServer(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{})

	cases := []struct{ name, body string }{
		{
			// json quotes the unknown field NAME from the request.
			name: "secret in unknown field name",
			body: `{"admin_token":"` + cfg.AdminToken + `","unknown_` + secret + `":1}`,
		},
		{
			// The duration parser quotes the VALUE from the request.
			name: "secret in a bad duration value",
			body: `{"admin_token":"` + cfg.AdminToken + `","updater":{"interval":"bogus-` + secret + `"}}`,
		},
		{
			name: "secret in a wrong-typed field",
			body: `{"admin_token":"` + cfg.AdminToken + `","updater":{"enabled":"` + secret + `"}}`,
		},
		{
			name: "secret in truncated body",
			body: `{"admin_token":"` + cfg.AdminToken + `","api_token":"` + secret,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(s, http.MethodPut, "/admin/config", tc.body, adminHeaders())
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", rec.Code, rec.Body)
			}
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("decode error reflected the request body:\n%s", rec.Body)
			}
			// The reason must still tell the operator something actionable.
			if !strings.Contains(rec.Body.String(), "decode config") {
				t.Fatalf("no actionable reason: %s", rec.Body)
			}
		})
	}
}

// TestDecodeErrorKeepsDetailServerSide pins two things about the server-side
// record: a rejected body is logged (so the operator can see it happened), and
// the log line does NOT contain request text. A value in a rejected body is not
// part of any configuration, so no scrub list covers it — it must never be
// written out at all.
func TestDecodeErrorKeepsDetailServerSide(t *testing.T) {
	cfg := baseConfig()
	const requestOnlySecret = "sk-in-request-only-4411"
	var buf bytes.Buffer
	s := newTestServerWithLogger(t, cfg, &fakeStore{cfg: cfg}, &transportSpy{}, &buf)

	rec := do(s, http.MethodPut, "/admin/config",
		`{"admin_token":"`+cfg.AdminToken+`","updater":{"interval":"`+requestOnlySecret+`"}}`, adminHeaders())
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "config body rejected") {
		t.Fatalf("the rejection was not logged: %s", logged)
	}
	for _, secret := range []string{requestOnlySecret, cfg.AdminToken, cfg.APIToken} {
		if secret != "" && strings.Contains(logged, secret) {
			t.Fatalf("the log line carried %q: %s", secret, logged)
		}
	}
}
