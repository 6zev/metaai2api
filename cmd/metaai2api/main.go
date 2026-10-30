// Command metaai2api serves an OpenAI-compatible API over a chat backend.
//
// The default backend is mock: it needs no credentials and answers
// deterministically, so the HTTP surface can be exercised before any meta.ai
// account is involved. The experimental-dgw backend is selected explicitly and
// refuses to start without its access token, golden vector and WASM codec; it
// never falls back to mock.
//
// The process shuts down gracefully on SIGINT and SIGTERM and logs its
// configuration with every secret redacted.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/config"
	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/codec"
	"github.com/me/metaai2api/internal/meta/mock"
	"github.com/me/metaai2api/internal/openai"
	"github.com/me/metaai2api/internal/redact"
	"github.com/me/metaai2api/internal/server"
	"github.com/me/metaai2api/internal/updater"
	"github.com/me/metaai2api/internal/version"
)

// defaultConfigPath is the value -h shows; it is not applied automatically, so
// starting without -config and without META_CONFIG uses defaults plus the
// environment, exactly as config.Load("") does.
const defaultConfigPath = "config.yaml"

// shutdownGrace bounds how long a graceful shutdown waits for in-flight
// requests before the listener is closed regardless.
const shutdownGrace = 10 * time.Second

// procSelfExe is what os.Executable resolves to inside a container.
const procSelfExe = "/proc/self/exe"

// targetEnv names the persistent binary path the OTA updater may replace inside
// a container; see updateTarget.
const targetEnv = "META_UPDATE_TARGET"

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))

	path, args := configPath(os.Args[1:], os.Getenv("META_CONFIG"))
	if wantsHelp(args) {
		usage(os.Stdout)
		return
	}
	cfg, err := loadConfig(path, args)
	if err != nil {
		slog.Error("load config", slog.String("error", err.Error()))
		os.Exit(2)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := serve(ctx, cfg, path); err != nil {
		slog.Error("fatal", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

// wantsHelp reports whether the arguments ask for usage instead of a run.
func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" || arg == "help" {
			return true
		}
	}
	return false
}

// usage prints the flags. Secrets come from the environment, so they are never
// named as flags here.
func usage(w io.Writer) {
	fmt.Fprintf(w, `metaai2api %s - OpenAI-compatible API over meta.ai (default backend: mock)

usage: metaai2api [-config path]

  -config path   YAML configuration file (e.g. %q, or $META_CONFIG).
                 When neither is set, defaults plus environment overrides apply.
  -h, --help     print this message

environment overrides: META_COOKIE, META_ACCESS_TOKEN, API_TOKEN, ADMIN_TOKEN,
BACKEND, ALL_PROXY, HTTP_PROXY, META_UPDATE_TARGET, DGW_CODEC_WASM.
`, version.Version, defaultConfigPath)
}

// configPath resolves the -config flag (or META_CONFIG) from raw arguments. It
// is a plain scan rather than flag.Parse so an unknown flag is reported here,
// not by flag's own os.Exit, and so the flag can also come from the environment.
// An empty result means "no file": configuration comes from defaults and
// environment overrides only, which is how a bare container image starts.
func configPath(args []string, env string) (string, []string) {
	path := env
	var rest []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-config" || arg == "--config":
			if i+1 < len(args) {
				path = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "-config="):
			path = strings.TrimPrefix(arg, "-config=")
		case strings.HasPrefix(arg, "--config="):
			path = strings.TrimPrefix(arg, "--config=")
		default:
			rest = append(rest, arg)
		}
	}
	return path, rest
}

// loadConfig reads the configuration. A non-empty path that cannot be read is
// an error (config.Load), so a typo in -config never silently starts with
// defaults. Unrecognised arguments are rejected rather than ignored.
func loadConfig(path string, args []string) (config.Config, error) {
	if len(args) > 0 {
		return config.Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(args, " "))
	}
	return config.Load(path)
}

// app is the wiring: the HTTP server and the resources it owns.
type app struct {
	handler *http.Server
	closers []func()
}

func (a *app) close() {
	for _, c := range a.closers {
		c()
	}
}

// buildApp wires the backend, the /v1 handler, the admin server and the
// optional updater into one http.Server. storePath is the config file
// PUT /admin/config persists to; empty means in-memory only.
func buildApp(cfg config.Config, storePath string) (*app, error) {
	b, err := newBackend(cfg, nil)
	if err != nil {
		return nil, err
	}

	a := &app{}
	if c, ok := b.(interface{ Close() error }); ok {
		a.closers = append(a.closers, func() { _ = c.Close() })
	}

	v1, err := openai.New(openai.Options{Backend: b, APIToken: cfg.APIToken})
	if err != nil {
		a.close()
		return nil, fmt.Errorf("v1 handler: %w", err)
	}

	h, err := server.New(server.Options{
		Config: cfg,
		Store:  fileStore{path: storePath, base: cfg},
		V1:     v1,
		Logger: slog.Default(),
	})
	if err != nil {
		a.close()
		return nil, err
	}
	a.closers = append(a.closers, func() { _ = h.Close() })

	if u := newUpdater(cfg); u != nil {
		h.SetUpdater(server.NewUpdaterFunc(u))
		h.SetUpdateChecker(server.NewUpdateChecker(u))
	}

	a.handler = &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return a, nil
}

// newBackend resolves the backend named by cfg. A backend that cannot start is
// an error, never a silent fallback to mock. codecFn overrides the codec
// constructor in tests.
func newBackend(cfg config.Config, codecFn func() (dgw.Codec, error)) (backend.ChatBackend, error) {
	switch cfg.Backend {
	case config.BackendMock:
		return mock.New(), nil
	case config.BackendExperimentalDGW:
		if codecFn == nil {
			// codec.New returns codec.Codec in the tagged build; adapt it so
			// the constructor has one type on every build.
			codecFn = func() (dgw.Codec, error) {
				c, err := codec.New()
				if err != nil {
					return nil, err
				}
				return c, nil
			}
		}
		missing := missingDGWPrerequisites(cfg, codecFn)
		if len(missing) > 0 {
			return nil, fmt.Errorf(
				"backend %s cannot start: missing %s. It is unverified against live meta.ai and there is no fallback to %s (see README.md, reference/README.md)",
				cfg.Backend, strings.Join(missing, "; "), config.BackendMock)
		}
		b, err := dgw.New(dgw.Options{
			Config:     dgwConfig(cfg),
			GoldenPath: cfg.Updater.DGWGoldenPath,
			Codec:      dgwCodec(codecFn),
		})
		if err != nil {
			return nil, fmt.Errorf("backend %s cannot start: %w (no fallback to %s)", cfg.Backend, err, config.BackendMock)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("backend %q is not supported", cfg.Backend)
	}
}

// missingDGWPrerequisites names every prerequisite experimental-dgw lacks, so a
// startup failure tells the operator the whole list instead of whichever check
// dgw.New happens to run first.
func missingDGWPrerequisites(cfg config.Config, newCodec func() (dgw.Codec, error)) []string {
	var missing []string
	if strings.TrimSpace(cfg.AccessToken) == "" {
		missing = append(missing, "access token (config access_token or META_ACCESS_TOKEN)")
	}
	if strings.TrimSpace(cfg.Updater.DGWGoldenPath) == "" {
		missing = append(missing, "golden vector (updater.dgw_golden_path), record one with cmd/dgwprobe")
	} else if _, err := dgw.LoadGolden(cfg.Updater.DGWGoldenPath); err != nil {
		missing = append(missing, fmt.Sprintf("a readable golden vector at %s: %v", cfg.Updater.DGWGoldenPath, err))
	}
	if _, err := newCodec(); err != nil {
		missing = append(missing, fmt.Sprintf("WASM codec (%v; %s)", err, codec.AssetHint()))
	}
	return missing
}

// dgwCodec binds the codec. dgw.New needs a non-nil Codec, and a bridge that
// cannot load must still report why, so a failed bind becomes a Codec whose
// calls all return the real error rather than a nil pointer.
func dgwCodec(newCodec func() (dgw.Codec, error)) dgw.Codec {
	c, err := newCodec()
	if err != nil {
		return &unavailableCodec{err: err}
	}
	return c
}

// unavailableCodec is a Codec whose every call reports the bind error, so
// dgw.New surfaces the missing module instead of a generic "no codec".
type unavailableCodec struct{ err error }

func (u *unavailableCodec) Encode(dgw.Frame) ([]byte, error)     { return nil, u.err }
func (u *unavailableCodec) Decode([]byte) (dgw.Frame, error)     { return dgw.Frame{}, u.err }
func (u *unavailableCodec) EncodePing() ([]byte, error)          { return nil, u.err }
func (u *unavailableCodec) EncodePong(dgw.Frame) ([]byte, error) { return nil, u.err }

// dgwConfig maps the service configuration onto the DGW backend's needs. A
// proxy URL that does not parse leaves ProxyURL nil, which is the direct dial;
// config.Validate has already rejected a malformed proxy_url.
func dgwConfig(cfg config.Config) dgw.Config {
	var proxyURL *url.URL
	if cfg.ProxyURL != "" {
		if u, err := url.Parse(cfg.ProxyURL); err == nil {
			proxyURL = u
		}
	}
	return dgw.Config{
		AccessToken: cfg.AccessToken,
		Cookie:      cfg.Cookie,
		AgentType:   cfg.AgentType,
		ProxyURL:    proxyURL,
	}
}

// fileStore persists admin configuration changes to the -config file.
type fileStore struct {
	path string
	base config.Config
}

// Load returns the initial configuration. The server owns the live copy.
func (s fileStore) Load() config.Config { return s.base }

// Save rewrites the config file atomically. An empty path means the operator
// started without a file, so the update is applied in memory only.
func (s fileStore) Save(next config.Config) error {
	if strings.TrimSpace(s.path) == "" {
		return nil
	}
	raw, err := yaml.Marshal(next)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".config-*.yaml")
	if err != nil {
		return err
	}
	name := f.Name()
	if _, err := f.Write(raw); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(name)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, s.path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// newUpdater returns the OTA updater for cfg, or nil when it is disabled or
// unconfigured.
func newUpdater(cfg config.Config) *updater.Updater {
	u := cfg.Updater
	if !u.Enabled || strings.TrimSpace(u.Owner) == "" || strings.TrimSpace(u.Repo) == "" {
		return nil
	}
	target, err := os.Executable()
	if err != nil {
		slog.Warn("updater: cannot locate executable, OTA disabled", slog.String("error", err.Error()))
		return nil
	}
	return updater.New(updater.Config{
		Owner:        u.Owner,
		Repo:         u.Repo,
		AssetPattern: u.AssetPattern,
		TargetPath:   updateTarget(os.Getenv(targetEnv), target),
		GOOS:         runtime.GOOS,
		GOARCH:       runtime.GOARCH,
	})
}

// updateTarget picks the file the updater may replace. Inside a container
// os.Executable is /proc/self/exe, and renaming over it does not change the
// binary a restart runs; when the operator names the persistent path in
// META_UPDATE_TARGET, that regular file is used instead. Anything else falls
// back to the executable so the updater's own checks report the real problem.
func updateTarget(configured, exe string) string {
	configured = strings.TrimSpace(configured)
	if configured == "" || !isProcSelfExe(exe) {
		return exe
	}
	st, err := os.Stat(configured)
	if err != nil || !st.Mode().IsRegular() {
		return exe
	}
	return configured
}

// isProcSelfExe reports whether exe is /proc/self/exe (or a path beneath it).
func isProcSelfExe(exe string) bool {
	exe = filepath.Clean(exe)
	return exe == procSelfExe || strings.HasPrefix(exe, procSelfExe+"/")
}

// pollSetup is the OTA poller configuration: how often to check, and how.
type pollSetup struct {
	interval time.Duration
}

// runner returns the function updater.Poll runs on every tick.
func (p *pollSetup) runner(u *updater.Updater) func(context.Context) {
	return func(ctx context.Context) {
		if _, err := u.Check(ctx); err != nil && ctx.Err() == nil {
			slog.Warn("updater: check failed", slog.String("error", err.Error()))
		}
	}
}

// pollConfig decides whether the OTA poller starts. It starts only for an
// enabled updater with an owner and repo; a non-positive interval is the
// updater package's own disable, so it is treated the same way.
func pollConfig(cfg config.Config) *pollSetup {
	u := cfg.Updater
	if !u.Enabled || strings.TrimSpace(u.Owner) == "" || strings.TrimSpace(u.Repo) == "" {
		return nil
	}
	if u.Interval.Std() <= 0 {
		return nil
	}
	return &pollSetup{interval: u.Interval.Std()}
}

// warnUnscrubbableSecrets reports configured secrets that are too short to be
// scrubbed from error text.
//
// Redaction refuses to substitute very short values, because substring
// replacement has no word boundary and would corrupt every message. The
// trade-off is that such a value is neither scrubbed from responses nor from
// logs, so the operator has to hear about it rather than assume it is covered.
func warnUnscrubbableSecrets(cfg config.Config) {
	secrets := server.Secrets(cfg)
	if usable := redact.Usable(secrets...); usable == len(secrets) {
		return
	}
	slog.Warn("some configured secrets are shorter than the redaction floor and will not be scrubbed",
		slog.Int("secrets", len(secrets)),
		slog.Int("scrubbable", redact.Usable(secrets...)),
		slog.Int("minimum_length", redact.MinSecretLen))
}

// startupLine renders the redacted startup configuration as logfmt.
func startupLine(cfg config.Config) string {
	r := cfg.Redacted()
	var b strings.Builder
	fmt.Fprintf(&b, "version=%q", version.Version)
	fmt.Fprintf(&b, " listen=%q", r.Listen)
	fmt.Fprintf(&b, " backend=%q", r.Backend)
	fmt.Fprintf(&b, " agent_type=%q", r.AgentType)
	fmt.Fprintf(&b, " cookie=%q", r.Cookie)
	fmt.Fprintf(&b, " access_token=%q", r.AccessToken)
	fmt.Fprintf(&b, " api_token=%q", r.APIToken)
	fmt.Fprintf(&b, " admin_token=%q", r.AdminToken)
	fmt.Fprintf(&b, " proxy_url=%q", r.ProxyURL)
	fmt.Fprintf(&b, " updater_enabled=%t", r.Updater.Enabled)
	return b.String()
}

// serve builds the app and serves until ctx is cancelled, then shuts down
// gracefully. A cancelled context is a clean exit, not an error.
func serve(ctx context.Context, cfg config.Config, storePath string) error {
	a, err := buildApp(cfg, storePath)
	if err != nil {
		return err
	}
	defer a.close()

	slog.Info("starting " + startupLine(cfg))
	warnUnscrubbableSecrets(cfg)

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen %s: %w", cfg.Listen, err)
	}

	// The updater is only polled when it is enabled and configured.
	if p := pollConfig(cfg); p != nil {
		if u := newUpdater(cfg); u != nil {
			slog.Info("updater: polling", slog.Duration("interval", p.interval))
			go updater.Poll(ctx, p.interval, p.runner(u))
		}
	}

	slog.Info("listening", slog.String("addr", ln.Addr().String()))

	errCh := make(chan error, 1)
	go func() {
		errCh <- a.handler.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := a.handler.Shutdown(shutCtx); err != nil {
			return fmt.Errorf("shutdown: %w", err)
		}
		if err := <-errCh; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// run is serve against the default store path; tests drive serve directly.
func run(ctx context.Context, cfg config.Config) error {
	return serve(ctx, cfg, "")
}
