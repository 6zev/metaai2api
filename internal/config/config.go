// Package config loads and validates the service configuration.
//
// Configuration comes from an optional YAML file plus environment overrides.
// Secrets are never logged: use Redacted for any output.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RedactedSecret is the placeholder substituted for every secret value.
const RedactedSecret = "***"

// Backend identifiers.
const (
	BackendMock            = "mock"
	BackendExperimentalDGW = "experimental-dgw"
)

// Config is the full service configuration.
type Config struct {
	Listen      string  `yaml:"listen"`
	APIToken    string  `yaml:"api_token"`
	AdminToken  string  `yaml:"admin_token"`
	Cookie      string  `yaml:"cookie"`
	AccessToken string  `yaml:"access_token"`
	AgentType   string  `yaml:"agent_type"`
	ProxyURL    string  `yaml:"proxy_url"`
	Backend     string  `yaml:"backend"`
	Updater     Updater `yaml:"updater"`
}

// Updater configures the OTA updater.
type Updater struct {
	Enabled       bool     `yaml:"enabled"`
	Owner         string   `yaml:"owner"`
	Repo          string   `yaml:"repo"`
	Interval      Duration `yaml:"interval"`
	AssetPattern  string   `yaml:"asset_pattern"`
	DGWGoldenPath string   `yaml:"dgw_golden_path"`
}

// Default returns the baseline configuration used when nothing is supplied.
func Default() Config {
	return Config{
		Listen:    ":8080",
		AgentType: "think_fast",
		Backend:   BackendMock,
		Updater: Updater{
			Enabled:      false,
			Owner:        "me",
			Repo:         "metaai2api",
			Interval:     Duration(24 * time.Hour),
			AssetPattern: "metaai2api-{os}-{arch}",
		},
	}
}

// Load reads defaults, then the YAML file at path (skipped when empty), then
// environment overrides, and validates the result. A non-empty path that does
// not exist is an error rather than a silent fallback.
func Load(path string) (Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read config %s: %w", path, err)
		}
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config %s: %w", path, err)
		}
	}

	applyEnv(&cfg)

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func applyEnv(cfg *Config) {
	overrides := []struct {
		env string
		dst *string
	}{
		{"META_COOKIE", &cfg.Cookie},
		{"META_ACCESS_TOKEN", &cfg.AccessToken},
		{"API_TOKEN", &cfg.APIToken},
		{"ADMIN_TOKEN", &cfg.AdminToken},
		{"BACKEND", &cfg.Backend},
	}
	for _, o := range overrides {
		if v := os.Getenv(o.env); v != "" {
			*o.dst = v
		}
	}

	// Proxy: explicit config wins over env; ALL_PROXY then HTTP_PROXY.
	if cfg.ProxyURL == "" {
		for _, env := range []string{"ALL_PROXY", "HTTP_PROXY"} {
			if v := os.Getenv(env); v != "" {
				cfg.ProxyURL = v
				break
			}
		}
	}
}

// Validate rejects configurations the server cannot run with.
func (c Config) Validate() error {
	if strings.TrimSpace(c.Listen) == "" {
		return errors.New("config: listen must not be empty")
	}
	switch c.Backend {
	case BackendMock, BackendExperimentalDGW:
	case "":
		return errors.New("config: backend must not be empty")
	default:
		return fmt.Errorf("config: unknown backend %q", c.Backend)
	}
	if c.ProxyURL != "" {
		u, err := url.Parse(c.ProxyURL)
		if err != nil {
			return fmt.Errorf("config: parse proxy_url: %w", err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("config: unsupported proxy_url scheme %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("config: proxy_url %q has no host", c.ProxyURL)
		}
	}
	if c.Cookie != "" {
		if _, err := ParseCookie(c.Cookie); err != nil {
			return fmt.Errorf("config: cookie: %w", err)
		}
	}
	if c.Backend == BackendExperimentalDGW && c.Updater.DGWGoldenPath == "" {
		return errors.New("config: backend experimental-dgw requires updater.dgw_golden_path")
	}
	return nil
}

// Redacted returns a copy safe to log or serve: every secret is replaced with
// RedactedSecret. The cookie keeps its key names so operators can see which
// cookies are configured, but no value survives. A proxy URL keeps its scheme,
// host and port because they are operationally useful, but any userinfo is
// stripped, since proxy credentials are secrets too.
func (c Config) Redacted() Config {
	out := c
	out.APIToken = redact(c.APIToken)
	out.AdminToken = redact(c.AdminToken)
	out.AccessToken = redact(c.AccessToken)
	out.Cookie = redactCookie(c.Cookie)
	out.ProxyURL = RedactProxyURL(c.ProxyURL)
	return out
}

// RedactProxyURL hides the credentials in a proxy URL while preserving its
// scheme, host and port. It is the single implementation used by config
// redaction, the admin surface and the startup log, so the three can never
// drift apart. An unparseable value is redacted whole.
func RedactProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return RedactedSecret
	}
	if u.User == nil {
		return raw
	}
	// Rebuild rather than use u.String(): setting User and printing the URL
	// percent-encodes the mask, so an operator reads "http://%2A%2A%2A@host"
	// instead of a readable marker. Splice the credentials out of the original
	// text, which also preserves any formatting the operator typed.
	if i := strings.LastIndex(raw, "@"); i >= 0 {
		scheme := raw[:strings.Index(raw, "://")+3]
		return scheme + RedactedSecret + raw[i:]
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return RedactedSecret
	}
	// A username with no password is not a credential; keep the URL intact.
	return raw
}

func redact(s string) string {
	if s == "" {
		return ""
	}
	return RedactedSecret
}

func redactCookie(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return ""
	}
	jar, err := ParseCookie(raw)
	if err != nil {
		// Unparseable means untrusted: hide everything rather than leak a fragment.
		return RedactedSecret
	}
	pairs := make([]string, 0, len(jar.Names()))
	for _, name := range jar.Names() {
		pairs = append(pairs, name+"="+RedactedSecret)
	}
	return strings.Join(pairs, "; ")
}

// Jar is an ordered cookie jar: it preserves the order cookies were supplied in,
// because the upstream extracts values positionally in places.
type Jar struct {
	names  []string
	values map[string]string
}

// ParseCookie parses a "Cookie:" header string such as "a=1; b=2" into an
// ordered Jar. Empty input yields an empty jar. Malformed pairs are errors.
func ParseCookie(raw string) (*Jar, error) {
	jar := &Jar{values: make(map[string]string)}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return jar, nil
	}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			// Tolerate a trailing separator, reject empty segments in the middle.
			continue
		}
		name, value, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		if !ok || name == "" {
			return nil, fmt.Errorf("malformed cookie pair %q", part)
		}
		if _, dup := jar.values[name]; !dup {
			jar.names = append(jar.names, name)
		}
		jar.values[name] = strings.TrimSpace(value)
	}
	return jar, nil
}

// Names returns cookie names in their original order.
func (j *Jar) Names() []string {
	out := make([]string, len(j.names))
	copy(out, j.names)
	return out
}

// Get returns the value of name.
func (j *Jar) Get(name string) (string, bool) {
	v, ok := j.values[name]
	return v, ok
}

// Len reports the number of cookies in the jar.
func (j *Jar) Len() int { return len(j.names) }

// CookieHeader renders the jar as a Cookie header value, preserving order.
func (j *Jar) CookieHeader() string {
	pairs := make([]string, 0, len(j.names))
	for _, name := range j.names {
		pairs = append(pairs, name+"="+j.values[name])
	}
	return strings.Join(pairs, "; ")
}
