package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/me/metaai2api/internal/config"
)

// TestNoConfigPathUsesDefaults pins the container default: with neither -config
// nor META_CONFIG, startup uses defaults plus the environment instead of
// demanding a file that a bare image does not contain.
func TestNoConfigPathUsesDefaults(t *testing.T) {
	path, args := configPath(nil, "")
	if path != "" {
		t.Fatalf("configPath with no flag/env = %q, want empty", path)
	}
	if len(args) != 0 {
		t.Fatalf("leftover args = %v, want none", args)
	}
	cfg, err := loadConfig(path, args)
	if err != nil {
		t.Fatalf("loadConfig with no file: %v", err)
	}
	if cfg.Backend != config.BackendMock {
		t.Fatalf("backend = %q, want mock", cfg.Backend)
	}

	// The flag wins over the environment, and the env wins over nothing.
	got, rest := configPath([]string{"-config", "/tmp/x.yaml", "stray"}, "/tmp/env.yaml")
	if got != "/tmp/x.yaml" || len(rest) != 1 || rest[0] != "stray" {
		t.Fatalf("configPath = %q, %v", got, rest)
	}
	if got, _ := configPath(nil, "/tmp/env.yaml"); got != "/tmp/env.yaml" {
		t.Fatalf("configPath ignored META_CONFIG: %q", got)
	}
}

// TestParseConfigFlag pins the -config flag: an absent config file is still an
// error (config.Load owns that rule), and a temp file is loaded verbatim.
func TestParseConfigFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.yaml")
	if err := os.WriteFile(path, []byte("listen: \":18080\"\nbackend: mock\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadConfig(path, []string{})
	if err != nil {
		t.Fatalf("loadConfig: %v", err)
	}
	if cfg.Listen != ":18080" {
		t.Fatalf("listen = %q, want :18080", cfg.Listen)
	}
	if cfg.Backend != config.BackendMock {
		t.Fatalf("backend = %q, want mock", cfg.Backend)
	}

	if _, err := loadConfig(filepath.Join(dir, "missing.yaml"), []string{}); err == nil {
		t.Fatal("a missing -config file must be an error, got nil")
	}
}

// TestDGWRequiresPrerequisites is the no-silent-fallback contract: asking for
// experimental-dgw without an access token, a golden vector or a codec fails
// loudly and names every missing piece.
func TestDGWRequiresPrerequisites(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = config.BackendExperimentalDGW
	cfg.AccessToken = ""
	cfg.Updater.DGWGoldenPath = ""

	_, err := newBackend(cfg, nil)
	if err == nil {
		t.Fatal("experimental-dgw without prerequisites must fail, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"access token", "golden vector", "codec"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q does not mention %q", msg, want)
		}
	}
	if strings.Contains(msg, "falling back") || strings.Contains(msg, "using mock") {
		t.Fatalf("error must not offer a fallback: %q", msg)
	}
}

// TestMockIsDefault holds the other side: the default config selects a backend
// with no credentials at all.
func TestMockIsDefault(t *testing.T) {
	cfg := config.Default()
	b, err := newBackend(cfg, nil)
	if err != nil {
		t.Fatalf("mock backend: %v", err)
	}
	if b.Name() != "mock" {
		t.Fatalf("backend name = %q, want mock", b.Name())
	}
}

// TestDGWMissingGoldenNamesThePath pins that the golden-vector failure reports
// the configured path so an operator can see what was looked for.
func TestDGWMissingGoldenNamesThePath(t *testing.T) {
	cfg := config.Default()
	cfg.Backend = config.BackendExperimentalDGW
	cfg.AccessToken = "token"
	cfg.Updater.DGWGoldenPath = filepath.Join(t.TempDir(), "nope.json")

	_, err := newBackend(cfg, nil)
	if err == nil {
		t.Fatal("missing golden vector must fail")
	}
	if !strings.Contains(err.Error(), "nope.json") {
		t.Fatalf("error %q does not name the golden path", err)
	}
}

// TestUpdateTargetIsThePodBinary pins the special case the updater cannot decide
// itself: when the process runs from /proc/self/exe (a replaced copy rather than
// an unlinked original, which is what a container restart leaves behind) and the
// conventional target exists, the target is used. Otherwise the executable is.
func TestUpdateTargetIsThePodBinary(t *testing.T) {
	dir := t.TempDir()
	pod := filepath.Join(dir, "metaai2api")
	if err := os.WriteFile(pod, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := updateTarget(pod, "/proc/self/exe")
	if got != pod {
		t.Fatalf("updateTarget = %q, want %q", got, pod)
	}

	if got := updateTarget("", "/usr/local/bin/metaai2api"); got != "/usr/local/bin/metaai2api" {
		t.Fatalf("updateTarget with no target = %q, want the executable", got)
	}
	if got := updateTarget(filepath.Join(dir, "absent"), "/usr/local/bin/metaai2api"); got != "/usr/local/bin/metaai2api" {
		t.Fatalf("updateTarget with a missing target = %q, want the executable", got)
	}
	if got := updateTarget(pod, "/usr/local/bin/metaai2api"); got != "/usr/local/bin/metaai2api" {
		t.Fatalf("updateTarget on a non-/proc executable = %q, want it", got)
	}
}

// TestServerServesHealthAndHidesAdmin runs the real wiring end to end: mock
// backend, no admin token, health 200 and /admin/config 404.
func TestServerServesHealthAndHidesAdmin(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"
	cfg.AdminToken = ""

	app, err := buildApp(cfg, "")
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer app.close()

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	srv := &http.Server{Handler: app.handler.Handler}
	done := make(chan struct{})
	go func() {
		_ = srv.Serve(ln)
		close(done)
	}()

	base := "http://" + ln.Addr().String()
	if code := get(t, base+"/admin/health"); code != http.StatusOK {
		t.Fatalf("/admin/health = %d, want 200", code)
	}
	if code := get(t, base+"/admin/config"); code != http.StatusNotFound {
		t.Fatalf("/admin/config = %d, want 404 when no admin token is set", code)
	}

	_ = srv.Shutdown(context.Background())
	<-done
}

// TestStreamsMockAnswer drives the streaming path through the mounted /v1
// handler and requires the SSE framing OpenAI clients expect.
func TestStreamsMockAnswer(t *testing.T) {
	cfg := config.Default()
	app, err := buildApp(cfg, "")
	if err != nil {
		t.Fatalf("buildApp: %v", err)
	}
	defer app.close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	srv := &http.Server{Handler: app.handler.Handler}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	body := `{"model":"meta-think-fast","stream":true,"messages":[{"role":"user","content":"ping"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, `"object":"chat.completion.chunk"`) {
		t.Fatalf("no chunk object in stream:\n%s", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "data: [DONE]") {
		t.Fatalf("stream does not end with [DONE]:\n%s", out)
	}
	// The mock splits its answer across several deltas, so the phrase only
	// appears once the deltas are reassembled in order.
	var answer strings.Builder
	for _, line := range strings.Split(out, "\n") {
		payload, ok := strings.CutPrefix(line, "data: ")
		if !ok || strings.TrimSpace(payload) == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("bad chunk %q: %v", payload, err)
		}
		for _, c := range chunk.Choices {
			answer.WriteString(c.Delta.Content)
		}
	}
	if !strings.HasPrefix(answer.String(), "mock response [think_fast]: ") {
		t.Fatalf("reassembled answer = %q, want the mock echo", answer.String())
	}
	if !strings.Contains(answer.String(), "ping") {
		t.Fatalf("answer %q does not echo the prompt", answer.String())
	}
}

// TestUpdaterPolledOnlyWhenEnabled pins requirement 3: the poller function is
// handed to the run loop only for a configured, enabled updater.
func TestUpdaterPolledOnlyWhenEnabled(t *testing.T) {
	if poll := pollConfig(config.Default()); poll != nil {
		t.Fatal("default config (updater disabled) must not start a poller")
	}

	cfg := config.Default()
	cfg.Updater.Enabled = true
	cfg.Updater.Owner = ""
	if poll := pollConfig(cfg); poll != nil {
		t.Fatal("enabled-but-unconfigured updater must not start a poller")
	}

	cfg.Updater = config.Default().Updater
	cfg.Updater.Enabled = true
	cfg.Updater.Owner = "me"
	cfg.Updater.Repo = "metaai2api"
	poll := pollConfig(cfg)
	if poll == nil {
		t.Fatal("enabled, configured updater must start a poller")
	}
	if poll.interval <= 0 {
		t.Fatalf("poll interval = %v, want positive", poll.interval)
	}
}

// TestRedactedStartupLine pins that nothing secret reaches the startup log.
func TestRedactedStartupLine(t *testing.T) {
	cfg := config.Default()
	cfg.Cookie = "datr=x; ecto_1_sess=supersecret"
	cfg.AccessToken = "token-secret"
	cfg.APIToken = "api-secret"
	cfg.AdminToken = "admin-secret"

	line := startupLine(cfg)
	if !strings.Contains(line, `backend="mock"`) {
		t.Fatalf("startup line lacks backend:\n%s", line)
	}
	for _, secret := range []string{"supersecret", "token-secret", "api-secret", "admin-secret"} {
		if strings.Contains(line, secret) {
			t.Fatalf("startup line leaks %q:\n%s", secret, line)
		}
	}
	if !strings.Contains(line, "datr=***") {
		t.Fatalf("startup line should keep the cookie names it masks:\n%s", line)
	}
}

func get(t *testing.T, url string) int {
	t.Helper()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// TestServeShutsDownCleanly pins the SIGINT/SIGTERM path: cancelling the
// context returns from serve without an error and without waiting for the
// grace period to expire.
func TestServeShutsDownCleanly(t *testing.T) {
	cfg := config.Default()
	cfg.Listen = "127.0.0.1:0"

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- serve(ctx, cfg, "") }()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not stop after the context was cancelled")
	}
}
