// Tests for the experimental DGW backend harness.
//
// Every test here runs against fixtures recorded in this repository or against
// an in-process httptest server. NO test asserts interoperability with the live
// meta.ai gateway: no DGW codec exists in this repository (the real one is a
// WebAssembly module that is not shipped), so a live session cannot be driven.
// The heartbeat test asserts this harness answers Ping with its own frame; that
// the frame carries Pong's type value, not a verified wire encoding.
package dgw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/dgw"
)

// fakeGateway is an in-process WebSocket server. It understands only the
// heartbeat frames this harness defines; it never asserts a real encoding.
type fakeGateway struct {
	upgrader websocket.Upgrader
	server   *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	received [][]byte
	ponged   chan struct{}
}

func newFakeGateway(t *testing.T) *fakeGateway {
	t.Helper()
	g := &fakeGateway{
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
		ponged:   make(chan struct{}, 1),
	}
	g.server = httptest.NewServer(http.HandlerFunc(g.handle))
	t.Cleanup(g.server.Close)
	return g
}

func (g *fakeGateway) URL() string {
	return "ws" + strings.TrimPrefix(g.server.URL, "http")
}

func (g *fakeGateway) handle(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.requests = append(g.requests, r.Clone(r.Context()))
	g.mu.Unlock()

	conn, err := g.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Greet, then answer a Ping with a frame the client must recognise as Pong.
	if err := conn.WriteMessage(websocket.BinaryMessage, dgw.TextFrame(dgw.FrameStreamGroupEstabStream)); err != nil {
		return
	}
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			return
		}
		g.mu.Lock()
		g.received = append(g.received, append([]byte(nil), msg...))
		g.mu.Unlock()

		if dgw.IsPingFrame(msg) {
			if err := conn.WriteMessage(websocket.BinaryMessage, dgw.TextFrame(dgw.FramePong)); err != nil {
				return
			}
			g.ponged <- struct{}{}
		}
	}
}

// requests records what the client sent during the handshake.
func (g *fakeGateway) handshakeRequest(t *testing.T) *http.Request {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.requests) == 0 {
		t.Fatal("gateway saw no handshake request")
	}
	return g.requests[0]
}

const goldenPath = "testdata/golden.example.json"

func testConfig() dgw.Config {
	return dgw.Config{
		Endpoint:       "",
		AccessToken:    "test-access-token",
		Cookie:         "ecto_1_sess=test-session",
		FBID:           "100000000000001",
		ConversationID: "conv-0001-stream",
		UserID:         "100000000000001",
		AgentType:      "think_fast",
	}
}

func TestBackendUnavailableWithoutGolden(t *testing.T) {
	b, err := dgw.New(dgw.Options{
		Config:     testConfig(),
		GoldenPath: "",
	})
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("New without golden error = %v, want ErrUnavailable", err)
	}
	if b != nil {
		t.Fatalf("New returned a backend (%T) with no golden vector", b)
	}

	missing := t.TempDir() + "/absent.json"
	b, err = dgw.New(dgw.Options{Config: testConfig(), GoldenPath: missing})
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("New with missing golden error = %v, want ErrUnavailable", err)
	}
	if b != nil {
		t.Fatalf("New returned a backend (%T) for a missing golden file", b)
	}
}

func TestBackendUnavailableWithoutCodec(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()

	b, err := dgw.New(dgw.Options{Config: cfg, GoldenPath: goldenPath})
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("New without codec error = %v, want ErrUnavailable", err)
	}
	if b != nil {
		t.Fatalf("New returned a backend (%T) with no codec", b)
	}
}

func TestBackendUnavailableWithoutAccessToken(t *testing.T) {
	cfg := testConfig()
	cfg.AccessToken = ""
	_, err := dgw.New(dgw.Options{Config: cfg, GoldenPath: goldenPath, Codec: &dgw.NopCodec{}})
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("New without access token error = %v, want ErrUnavailable", err)
	}
}

func TestNewWithGoldenAndCodec(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()

	b, err := dgw.New(dgw.Options{Config: cfg, GoldenPath: goldenPath, Codec: &dgw.NopCodec{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if b == nil {
		t.Fatal("New returned a nil backend")
	}
	if got := b.Name(); got != "experimental-dgw" {
		t.Fatalf("Name() = %q, want %q", got, "experimental-dgw")
	}
}

func TestHandshakeURLAndHeaders(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()

	conn, err := dgw.Dial(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	req := gw.handshakeRequest(t)
	if got, want := req.URL.Path, "/ws/clippy"; got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	wantQuery := map[string]string{
		"x-dgw-appid":      "1522763855472543",
		"x-dgw-appversion": "1.0.0",
		"x-dgw-authtype":   "15:0",
		"x-dgw-version":    "5",
		"x-dgw-uuid":       "100000000000001",
		"x-dgw-tier":       "prod",
		"Authorization":    "test-access-token",
		"x-dgw-app-origin": "meta.ai",
	}
	q := req.URL.Query()
	for k, want := range wantQuery {
		if got := q.Get(k); got != want {
			t.Errorf("query %s = %q, want %q", k, got, want)
		}
	}
	if _, ok := req.URL.Query()["x-dgw-establish-stream-frame-base64"]; !ok {
		t.Error("handshake has no x-dgw-establish-stream-frame-base64 parameter")
	}
	if got := req.Header.Get("x-ecto-conversation-id"); got != cfg.ConversationID {
		t.Errorf("x-ecto-conversation-id = %q, want %q", got, cfg.ConversationID)
	}
	if got := req.Header.Get("Cookie"); got != cfg.Cookie {
		t.Errorf("Cookie = %q, want %q", got, cfg.Cookie)
	}
	if got := req.Header.Get("Origin"); got != "https://meta.ai" {
		t.Errorf("Origin = %q, want %q", got, "https://meta.ai")
	}
}

func TestConversationGroupingHeader(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()
	cfg.ConversationID = ""

	conn, err := dgw.Dial(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	if got := gw.handshakeRequest(t).Header.Get("x-ecto-conversation-id"); got != "" {
		t.Fatalf("x-ecto-conversation-id = %q, want empty", got)
	}
}

func TestPingPong(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()

	conn, err := dgw.Dial(context.Background(), cfg, nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()

	// The server greets with an EstabStream frame; drain it so the heartbeat
	// reads the Pong it triggers rather than the greeting.
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read greeting: %v", err)
	}

	got, err := dgw.Heartbeat(context.Background(), conn, nil)
	if err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got.Type != dgw.FramePong {
		t.Fatalf("Heartbeat returned %v, want %v", got.Type, dgw.FramePong)
	}
	if !got.Type.Known() || got.Type != dgw.FramePong {
		t.Fatalf("reply type %v is not Pong", got.Type)
	}

	gw.mu.Lock()
	received := append([][]byte(nil), gw.received...)
	gw.mu.Unlock()
	if len(received) != 1 {
		t.Fatalf("gateway received %d frames, want 1", len(received))
	}
	if !dgw.IsPingFrame(received[0]) {
		t.Fatalf("gateway received %x, which is not the harness's Ping frame", received[0])
	}

	select {
	case <-gw.ponged:
	case <-time.After(time.Second):
		t.Fatal("gateway never answered the Ping")
	}
}

func TestUnmappedFrameIsTypedError(t *testing.T) {
	if _, err := dgw.Classify(dgw.TextFrame(dgw.FrameType(11))); !errors.Is(err, dgw.ErrUnmappedFrame) {
		t.Fatalf("Classify(unmapped) error = %v, want ErrUnmappedFrame", err)
	}
	if _, err := dgw.Classify(dgw.TextFrame(dgw.FramePing)); err != nil {
		t.Fatalf("Classify(Ping) error = %v", err)
	}
	if _, err := dgw.Classify(dgw.TextFrame(dgw.FrameDeauth)); !errors.Is(err, dgw.ErrStreamTerminated) {
		t.Fatalf("Classify(Deauth) error = %v, want ErrStreamTerminated", err)
	}
}

func TestDeprecatedFramesAreTypedErrors(t *testing.T) {
	for _, ft := range []dgw.FrameType{
		dgw.FrameDeprecatedEstabStream, dgw.FrameDeprecatedData, dgw.FrameDeprecatedEndOfData,
	} {
		if _, err := dgw.Classify(dgw.TextFrame(ft)); !errors.Is(err, dgw.ErrDeprecatedFrame) {
			t.Fatalf("Classify(%v) error = %v, want ErrDeprecatedFrame", ft, err)
		}
	}
}

func TestTerminalFrames(t *testing.T) {
	for _, ft := range []dgw.FrameType{dgw.FrameDrain, dgw.FrameDeauth, dgw.FrameStreamGroupEndOfData, dgw.FrameDeprecatedEndOfData} {
		if !ft.Terminal() {
			t.Errorf("%v is not terminal", ft)
		}
	}
	for _, ft := range []dgw.FrameType{dgw.FramePing, dgw.FramePong, dgw.FrameStreamGroupData} {
		if ft.Terminal() {
			t.Errorf("%v is terminal", ft)
		}
	}
}

func TestRedactURL(t *testing.T) {
	cfg := testConfig()
	raw, err := dgw.ConnectURL(cfg, nil)
	if err != nil {
		t.Fatalf("ConnectURL: %v", err)
	}
	got := dgw.RedactURL(raw)
	if strings.Contains(got, cfg.AccessToken) {
		t.Fatalf("RedactURL leaked the access token: %s", got)
	}
	if !strings.Contains(got, "Authorization=%2A%2A%2A") {
		t.Fatalf("RedactURL %q does not mask Authorization", got)
	}
	if got := dgw.RedactURL("::not a url::"); got != "<invalid url>" {
		t.Fatalf("RedactURL(invalid) = %q", got)
	}
}

func TestConnectURLRejectsNonWS(t *testing.T) {
	cfg := testConfig()
	cfg.Endpoint = "https://example.invalid/ws"
	if _, err := dgw.ConnectURL(cfg, nil); err == nil {
		t.Fatal("ConnectURL accepted an https endpoint")
	}
}

func TestConnectURLEstablishParam(t *testing.T) {
	cfg := testConfig()
	raw, err := dgw.ConnectURL(cfg, []byte{0x0a, 0x03})
	if err != nil {
		t.Fatalf("ConnectURL: %v", err)
	}
	want := "CgM="
	if !strings.Contains(raw, "x-dgw-establish-stream-frame-base64="+url.QueryEscape(want)) &&
		!strings.Contains(raw, "x-dgw-establish-stream-frame-base64="+want) {
		t.Fatalf("ConnectURL %q does not carry the establish frame %q", raw, want)
	}
}

func TestDialRoutesThroughProxy(t *testing.T) {
	gw := newFakeGateway(t)
	cfg := testConfig()
	cfg.Endpoint = gw.URL()
	// A socks5 proxy on a closed port: if the dial bypassed the proxy it would
	// reach the httptest server and succeed.
	cfg.ProxyURL = &url.URL{Scheme: "socks5", Host: "127.0.0.1:1"}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := dgw.Dial(ctx, cfg, nil)
	if err == nil {
		conn.Close()
		t.Fatal("Dial succeeded despite a dead socks5 proxy: the proxy was bypassed")
	}
}

func TestConnectURLDefaultPath(t *testing.T) {
	cfg := testConfig()
	cfg.Endpoint = ""
	raw, err := dgw.ConnectURL(cfg, nil)
	if err != nil {
		t.Fatalf("ConnectURL: %v", err)
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got, want := u.Scheme+"://"+u.Host+u.Path, "wss://gateway.meta.ai/ws/clippy"; got != want {
		t.Fatalf("default endpoint = %q, want %q", got, want)
	}

	cfg.Endpoint = "wss://gw.example/ws"
	raw, err = dgw.ConnectURL(cfg, nil)
	if err != nil {
		t.Fatalf("ConnectURL: %v", err)
	}
	if u, _ := url.Parse(raw); u.Path != "/ws/clippy" {
		t.Fatalf("endpoint already ending in /ws gave path %q", u.Path)
	}
}
