package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const rawSessionToken = "supersecretsessiontoken123"

func TestCookieRoundTrip(t *testing.T) {
	cookie := "datr=abc; ecto_1_sess=" + rawSessionToken + "; wd=xyz; dpr=auto; rd_challenge=ch"

	jar, err := ParseCookie(cookie)
	if err != nil {
		t.Fatalf("ParseCookie: %v", err)
	}

	want := []string{"datr", "ecto_1_sess", "wd", "dpr", "rd_challenge"}
	got := jar.Names()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordered jar names = %v, want %v", got, want)
	}

	if v, ok := jar.Get("ecto_1_sess"); !ok || v != rawSessionToken {
		t.Fatalf("Get(ecto_1_sess) = %q, %v", v, ok)
	}

	header := jar.CookieHeader()
	if !strings.Contains(header, "ecto_1_sess="+rawSessionToken) {
		t.Fatalf("CookieHeader() missing session token: %q", header)
	}
	if strings.Contains(header, " ") == false {
		t.Fatalf("CookieHeader() should join pairs with \"; \": %q", header)
	}

	// Round-trip: parsing the header back must reproduce the same ordered pairs.
	again, err := ParseCookie(header)
	if err != nil {
		t.Fatalf("ParseCookie(CookieHeader()): %v", err)
	}
	if again.CookieHeader() != header {
		t.Fatalf("round trip mismatch:\n got %q\nwant %q", again.CookieHeader(), header)
	}
}

func TestParseCookieRejectsMalformed(t *testing.T) {
	for _, in := range []string{"novalue", "a=1; broken"} {
		jar, err := ParseCookie(in)
		if err == nil {
			t.Fatalf("ParseCookie(%q) expected error, got jar %v", in, jar)
		}
	}
}

func TestRedactedHidesSecrets(t *testing.T) {
	cfg := Default()
	cfg.Cookie = "datr=abc; ecto_1_sess=" + rawSessionToken + "; wd=xyz"
	cfg.AccessToken = "accesstoken-abc-987"
	cfg.APIToken = "apitoken-xyz-555"
	cfg.AdminToken = "admintoken-qrs-444"

	red := cfg.Redacted()

	// The raw session token must not appear anywhere in the redacted copy.
	for _, secret := range []string{rawSessionToken, "accesstoken-abc-987", "apitoken-xyz-555", "admintoken-qrs-444"} {
		if strings.Contains(red.Cookie, secret) ||
			strings.Contains(red.AccessToken, secret) ||
			strings.Contains(red.APIToken, secret) ||
			strings.Contains(red.AdminToken, secret) {
			t.Fatalf("Redacted() leaked secret %q: %+v", secret, red)
		}
	}

	if red.Cookie == "***" {
		t.Fatalf("Redacted() should keep the cookie jar structure, got just ***")
	}

	// The original must be untouched.
	if cfg.AccessToken != "accesstoken-abc-987" {
		t.Fatalf("Redacted() mutated the original config")
	}
}

// TestRedactedHidesProxyCredentials covers the field that is easiest to forget:
// proxy userinfo arrives from HTTP_PROXY/ALL_PROXY and must not survive
// redaction, while the host and port stay visible for diagnosis.
func TestRedactedHidesProxyCredentials(t *testing.T) {
	cases := []struct {
		name      string
		proxyURL  string
		secrets   []string
		preserved []string
	}{
		{"http with password", "http://alice:proxy-password-123@10.0.0.1:3128",
			[]string{"proxy-password-123"}, []string{"10.0.0.1:3128"}},
		{"socks5 with password", "socks5://bob:socks-secret-456@proxy.local:1080",
			[]string{"socks-secret-456"}, []string{"proxy.local:1080"}},
		{"encoded password", "http://carol:p%40ss%2Fword@203.0.113.9:8080",
			[]string{"p@ss/word", "p%40ss%2Fword"}, []string{"203.0.113.9:8080"}},
		{"no credentials", "http://10.0.0.5:3128", nil, []string{"10.0.0.5:3128"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.ProxyURL = tc.proxyURL
			got := cfg.Redacted().ProxyURL
			for _, s := range tc.secrets {
				if strings.Contains(got, s) {
					t.Fatalf("Redacted().ProxyURL leaked %q: %q", s, got)
				}
			}
			for _, s := range tc.preserved {
				if !strings.Contains(got, s) {
					t.Fatalf("Redacted().ProxyURL dropped %q: %q", s, got)
				}
			}
			if cfg.ProxyURL != tc.proxyURL {
				t.Fatalf("Redacted() mutated the original proxy URL")
			}
		})
	}

	if got := (Config{ProxyURL: "://not a url"}).Redacted().ProxyURL; got != RedactedSecret {
		t.Fatalf("unparseable proxy URL = %q, want %q", got, RedactedSecret)
	}
}

func TestRedactedEmptyStaysEmpty(t *testing.T) {
	cfg := Default()
	red := cfg.Redacted()
	if red.Cookie != "" || red.AccessToken != "" || red.APIToken != "" || red.AdminToken != "" {
		t.Fatalf("empty secrets should stay empty, got %+v", red)
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\"): %v", err)
	}
	if cfg.Listen != ":8080" {
		t.Fatalf("Listen = %q, want :8080", cfg.Listen)
	}
	if cfg.Backend != "mock" {
		t.Fatalf("Backend = %q, want mock", cfg.Backend)
	}
	if cfg.AgentType != "think_fast" {
		t.Fatalf("AgentType = %q, want think_fast", cfg.AgentType)
	}
	if cfg.Updater.Interval == 0 {
		t.Fatalf("Updater.Interval default should be non-zero")
	}
}

func TestLoadFileAndEnvOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `listen: ":9090"
backend: mock
agent_type: think_hard
cookie: "datr=abc; ecto_1_sess=` + rawSessionToken + `"
access_token: from-file
updater:
  enabled: true
  owner: me
  repo: metaai2api
  interval: 6h
  asset_pattern: "metaai2api-{os}-{arch}"
  dgw_golden_path: /tmp/golden.json
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("META_ACCESS_TOKEN", "from-env")
	t.Setenv("BACKEND", "experimental-dgw")
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:3128")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Listen != ":9090" {
		t.Fatalf("Listen = %q", cfg.Listen)
	}
	if cfg.AgentType != "think_hard" {
		t.Fatalf("AgentType = %q", cfg.AgentType)
	}
	if cfg.AccessToken != "from-env" {
		t.Fatalf("AccessToken = %q, want env override", cfg.AccessToken)
	}
	if cfg.Backend != "experimental-dgw" {
		t.Fatalf("Backend = %q, want env override", cfg.Backend)
	}
	if cfg.ProxyURL != "http://127.0.0.1:3128" {
		t.Fatalf("ProxyURL = %q", cfg.ProxyURL)
	}
	if !cfg.Updater.Enabled || cfg.Updater.Owner != "me" || cfg.Updater.DGWGoldenPath != "/tmp/golden.json" {
		t.Fatalf("updater not decoded: %+v", cfg.Updater)
	}
	if cfg.Updater.Interval.Std().Hours() != 6 {
		t.Fatalf("Interval = %v", cfg.Updater.Interval)
	}
}

func TestLoadRejectsUnknownBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("backend: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for unknown backend")
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.Listen = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty listen")
	}

	cfg = Default()
	cfg.Backend = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for empty backend")
	}

	cfg = Default()
	cfg.ProxyURL = "://bad"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected error for malformed proxy url")
	}

	if err := Default().Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
}

func TestExampleConfigLoads(t *testing.T) {
	// The shipped example must parse and validate with no env overrides.
	for _, env := range []string{"META_COOKIE", "META_ACCESS_TOKEN", "API_TOKEN", "ADMIN_TOKEN", "BACKEND", "ALL_PROXY", "HTTP_PROXY"} {
		t.Setenv(env, "")
	}
	cfg, err := Load("../../config.example.yaml")
	if err != nil {
		t.Fatalf("example config does not load: %v", err)
	}
	if cfg.Backend != "mock" {
		t.Fatalf("example backend = %q, want mock", cfg.Backend)
	}
	if cfg.Cookie != "" || cfg.AccessToken != "" || cfg.APIToken != "" || cfg.AdminToken != "" {
		t.Fatalf("example config must not carry secrets: %+v", cfg.Redacted())
	}
}

// TestValidateDoesNotQuoteProxyCredentials pins the inner defence: Validate's
// error text is returned to clients by PUT /admin/config, so it must never
// contain the raw proxy URL. The admin layer scrubs as well; this test covers
// the case where someone calls Validate directly.
func TestValidateDoesNotQuoteProxyCredentials(t *testing.T) {
	const pw = "sk-live-inner-layer-check-99"
	cfg := Default()
	cfg.ProxyURL = "http://alice:" + pw + "@"

	err := cfg.Validate()
	if err == nil {
		t.Fatal("a proxy URL with no host must fail validation")
	}
	if strings.Contains(err.Error(), pw) {
		t.Fatalf("Validate leaked the proxy password: %v", err)
	}
	if !strings.Contains(err.Error(), "no host") {
		t.Fatalf("Validate lost the useful detail: %v", err)
	}
	// The host-less URL is still diagnosable from the redacted form.
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("Validate did not quote a redacted URL: %v", err)
	}
}
