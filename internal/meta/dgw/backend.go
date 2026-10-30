// Experimental DGW backend: handshake, heartbeat and frame classification.
//
// The frame wire format is UNVERIFIED. The real codec is a protobuf
// implementation compiled to WebAssembly (dgwcppbridge.wasm) and is not
// available in this repository, so this file never encodes or decodes a real
// frame. Frame bytes come from two places only: a recorded golden vector, or a
// Codec handed in by the caller (task 11 supplies the wazero-backed one).
//
// A Backend only starts when it has both an access token and a golden vector;
// without either it fails with backend.ErrUnavailable. Nothing here is verified
// for interoperability with the live gateway.
package dgw

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/proxy"
)

const (
	// DefaultEndpoint is the DGW gateway.
	DefaultEndpoint = "wss://gateway.meta.ai/ws"
	// ServiceID is appended to the endpoint path (the site sends "/clippy").
	ServiceID = "clippy"

	// AppID and AppVersion go in the handshake query.
	AppID      = "1522763855472543"
	AppVersion = "1.0.0"
	// AuthType and ProtocolVersion go in the handshake query.
	AuthType        = "15:0"
	ProtocolVersion = "5"
	// Tier and AppOrigin go in the handshake query.
	Tier      = "prod"
	AppOrigin = "meta.ai"
	// Origin is the browser Origin header on the handshake.
	Origin = "https://meta.ai"

	// ConnectTimeout bounds the WebSocket handshake; the site sets 20000ms.
	ConnectTimeout = 20 * time.Second
	// KeepAlive is the interval the site sends heartbeat frames at.
	KeepAlive = 30 * time.Second
	// HeartbeatTimeout bounds one heartbeat exchange when the caller's context
	// has no deadline of its own.
	HeartbeatTimeout = 10 * time.Second

	handshakeHeader = "x-dgw-establish-stream-frame-base64"
	conversationKey = "x-ecto-conversation-id"
	userAgent       = "Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0"
)

// backendName is the Name() value, matching config.BackendExperimentalDGW.
const backendName = "experimental-dgw"

// Config is what the backend needs to reach the gateway. Every field has a
// documented meaning from the media session capture; none of it is verified
// against the live gateway.
type Config struct {
	// Endpoint overrides DefaultEndpoint (scheme and host, e.g. a ws:// from
	// httptest); ConnectURL appends "/ws/clippy" to its path.
	Endpoint string
	// AccessToken is the raw Authorization query value.
	AccessToken string
	// Cookie is the meta.ai Cookie header; the gateway handshake carries it.
	Cookie string
	// FBID is the viewer's fb id, sent as x-dgw-uuid.
	FBID string
	// ConversationID groups the stream; empty omits the header.
	ConversationID string
	// UserID and AgentType appear in the client-built payload.
	UserID    string
	AgentType string
	// ProxyURL is the proxy the dial goes through. nil or empty dials direct
	// through the proxy package's current transport configuration.
	ProxyURL *url.URL
}

// Options configures New.
type Options struct {
	Config Config
	// GoldenPath is the recorded-vector file. It is required.
	GoldenPath string
	// Codec is the frame codec. It is required.
	Codec Codec
}

// Backend is the experimental DGW ChatBackend. It holds a validated golden set
// and a codec; frame exchange is delegated to the codec.
type Backend struct {
	cfg    Config
	golden *GoldenSet
	codec  Codec

	mu   sync.Mutex
	conn *websocket.Conn
}

// unavailablef reports a backend that cannot run. The result matches both
// backend.ErrUnavailable and this package's errUnavailable, keeping the detail.
func unavailablef(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errUnavailable, fmt.Sprintf(format, args...))
}

// New validates opts and returns a backend ready to dial. It returns
// backend.ErrUnavailable (and this package's errUnavailable) when the access
// token is empty, the golden vector cannot be loaded, or no codec is supplied.
func New(opts Options) (*Backend, error) {
	if strings.TrimSpace(opts.Config.AccessToken) == "" {
		return nil, unavailablef("access token is empty")
	}
	golden, err := LoadGolden(opts.GoldenPath)
	if err != nil {
		return nil, unavailablef("golden vector: %v", err)
	}
	if opts.Codec == nil {
		return nil, unavailablef("no codec: the DGW codec is a WASM module that is not shipped")
	}
	return &Backend{cfg: opts.Config, golden: golden, codec: opts.Codec}, nil
}

// Name returns the backend identifier used in configuration.
func (b *Backend) Name() string { return backendName }

// Complete is not implemented: the DGW path is stream-only.
func (b *Backend) Complete(context.Context, backend.ChatRequest) (backend.ChatResult, error) {
	return backend.ChatResult{}, fmt.Errorf("%w: DGW is stream-only", backend.ErrNotImplemented)
}

// Stream is not implemented. Replaying a recorded vector is not an answer, and
// a real exchange needs the WASM codec (task 11).
func (b *Backend) Stream(ctx context.Context, _ backend.ChatRequest, _ func(backend.Delta) error) error {
	return fmt.Errorf("%w: DGW stream needs the WASM codec bridge", backend.ErrNotImplemented)
}

// Golden returns the set the backend was started with.
func (b *Backend) Golden() *GoldenSet { return b.golden }

// ConnectURL builds the handshake URL for cfg.
//
// It reproduces constructConnectUrl from 2umt4-28uxtjh.js: the endpoint, plus
// "/ws" and the service id, plus the x-dgw-* query parameters, Authorization and
// the establish-stream frame. establishFrame is recorded bytes supplied by the
// caller (a golden vector); nil still sets the parameter, empty, because this
// package cannot produce a real frame.
func ConnectURL(cfg Config, establishFrame []byte) (string, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = DefaultEndpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("dgw: parse endpoint %q: %w", endpoint, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("dgw: endpoint %q is not ws or wss", endpoint)
	}

	u.Path = servicePath(u.Path)

	q := url.Values{}
	q.Set("x-dgw-appid", AppID)
	q.Set("x-dgw-appversion", AppVersion)
	q.Set("x-dgw-authtype", AuthType)
	q.Set("x-dgw-version", ProtocolVersion)
	q.Set("x-dgw-uuid", cfg.FBID)
	q.Set("x-dgw-tier", Tier)
	q.Set("Authorization", cfg.AccessToken)
	q.Set("x-dgw-app-origin", AppOrigin)
	q.Set(handshakeHeader, base64.StdEncoding.EncodeToString(establishFrame))
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// HandshakeRequestHeaders returns the non-query headers the handshake carries.
func HandshakeRequestHeaders(cfg Config) http.Header {
	h := http.Header{}
	if cfg.Cookie != "" {
		h.Set("Cookie", cfg.Cookie)
	}
	h.Set("Origin", Origin)
	h.Set("User-Agent", userAgent)
	if cfg.ConversationID != "" {
		h.Set(conversationKey, cfg.ConversationID)
	}
	return h
}

// servicePath returns base with the DGW service path appended: the site's
// endpoint is wss://gateway.meta.ai/ws and constructConnectUrl appends
// "/clippy". An empty base becomes "/ws/clippy"; a base already ending in "/ws"
// gets just "/clippy"; a base already ending in the full service path is left
// alone. Callers can therefore pass the host, the "/ws" endpoint or the final
// URL.
func servicePath(base string) string {
	want := "/ws/" + ServiceID
	base = strings.TrimRight(base, "/")
	switch {
	case base == "", base == "/ws":
		if base == "" {
			return want
		}
		return base + "/" + ServiceID
	case strings.HasSuffix(base, want):
		return base
	}
	return base + want
}

// Dial opens the gateway connection with an explicit establish-stream frame
// (nil sends none). Every dial goes through the proxy package's dialer, never
// http.DefaultTransport; the WebSocket handshake is bounded by ConnectTimeout.
func Dial(ctx context.Context, cfg Config, establish []byte) (*websocket.Conn, error) {
	target, err := ConnectURL(cfg, establish)
	if err != nil {
		return nil, err
	}

	dialCtx, cancel := context.WithTimeout(ctx, ConnectTimeout)
	defer cancel()

	dialer := &websocket.Dialer{HandshakeTimeout: ConnectTimeout}
	if err := dialerThroughProxy(dialer, cfg.ProxyURL); err != nil {
		return nil, err
	}

	conn, resp, err := dialer.DialContext(dialCtx, target, HandshakeRequestHeaders(cfg))
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("dgw: handshake %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("dgw: handshake: %w", err)
	}
	return conn, nil
}

// dialerThroughProxy points wsDialer at the connection the proxy package
// returns. A scheme it cannot back with a raw dialer (http/https) is an error,
// not a silent direct dial.
func dialerThroughProxy(wsDialer *websocket.Dialer, proxyURL *url.URL) error {
	d, err := proxy.Dialer(proxyURL)
	if err != nil {
		return fmt.Errorf("dgw: proxy: %w", err)
	}
	if cd, ok := d.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	}); ok {
		wsDialer.NetDialContext = cd.DialContext
		return nil
	}
	wsDialer.NetDial = d.Dial
	return nil
}

// Heartbeat sends one heartbeat frame and returns the reply.
//
// Without a codec the harness sends HeartbeatFrame and expects the peer to
// answer with a Pong-classified frame. With a codec it delegates both directions
// entirely to the codec. A reply that is not a Pong, or a read that fails, is a
// typed error; nothing is swallowed. With no ctx deadline the exchange is bounded
// by HeartbeatTimeout so a silent peer cannot hang the caller.
func Heartbeat(ctx context.Context, conn *websocket.Conn, codec Codec) (Frame, error) {
	if conn == nil {
		return Frame{}, errors.New("dgw: heartbeat: nil connection")
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, HeartbeatTimeout)
		defer cancel()
	}
	out := HeartbeatFrame()
	payload := out.Raw
	if codec != nil {
		var err error
		payload, err = codec.EncodePing()
		if err != nil {
			return Frame{}, fmt.Errorf("dgw: heartbeat: encode ping: %w", err)
		}
	} else {
		payload = TextFrame(FramePing)
	}

	if err := writeWithContext(ctx, conn, payload); err != nil {
		return Frame{}, err
	}
	raw, err := readWithContext(ctx, conn)
	if err != nil {
		return Frame{}, err
	}

	if codec != nil {
		reply, err := codec.Decode(raw)
		if err != nil {
			return Frame{}, fmt.Errorf("dgw: heartbeat: decode reply: %w", err)
		}
		if reply.Type != FramePong {
			return Frame{}, &FrameError{Type: reply.Type, Err: ErrUnmappedFrame, Detail: "heartbeat reply is not Pong"}
		}
		return reply, nil
	}

	ft, err := Classify(raw)
	if err != nil {
		return Frame{}, err
	}
	if ft != FramePong {
		return Frame{}, &FrameError{Type: ft, Err: ErrUnmappedFrame, Detail: "heartbeat reply is not Pong"}
	}
	return Frame{Direction: DirectionInbound, Type: FramePong, Raw: raw}, nil
}

func writeWithContext(ctx context.Context, conn *websocket.Conn, payload []byte) error {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return fmt.Errorf("dgw: set write deadline: %w", err)
		}
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, payload); err != nil {
		return fmt.Errorf("dgw: write heartbeat: %w", err)
	}
	return nil
}

func readWithContext(ctx context.Context, conn *websocket.Conn) ([]byte, error) {
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("dgw: set read deadline: %w", err)
		}
	}
	_, raw, err := conn.ReadMessage()
	if err != nil {
		return nil, fmt.Errorf("dgw: read heartbeat reply: %w", err)
	}
	return raw, nil
}

// Classify maps a harness heartbeat-encoded payload to its frame type, failing
// with typedd errors for the cases that must never be swallowed: an unmapped,
// deprecated or terminal frame.
func Classify(raw []byte) (FrameType, error) {
	t, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		return 0, &FrameError{Err: ErrUnmappedFrame, Detail: fmt.Sprintf("payload %q is not a frame type", raw)}
	}
	ft := FrameType(t)
	if !ft.Known() {
		return 0, &FrameError{Type: ft, Err: ErrUnmappedFrame}
	}
	switch ft {
	case FrameDeprecatedEstabStream, FrameDeprecatedData, FrameDeprecatedEndOfData:
		return ft, &FrameError{Type: ft, Err: ErrDeprecatedFrame}
	case FrameDrain, FrameDeauth, FrameStreamGroupEndOfData:
		return ft, &FrameError{Type: ft, Err: ErrStreamTerminated}
	}
	return ft, nil
}

// Dial opens the gateway with the backend's recorded establish frame.
func (b *Backend) Dial(ctx context.Context) (*websocket.Conn, error) {
	conn, err := Dial(ctx, b.cfg, b.establishFrame())
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.conn = conn
	b.mu.Unlock()
	return conn, nil
}

// establishFrame returns the recorded establish-stream payload, or nil.
func (b *Backend) establishFrame() []byte {
	for _, v := range b.golden.Frames {
		if v.Direction != DirectionInbound {
			continue
		}
		if ft, ok := ParseFrameTypeName(v.Type); ok && ft == FrameStreamGroupEstabStream {
			raw, err := base64.StdEncoding.DecodeString(v.RawB64)
			if err != nil {
				continue
			}
			return raw
		}
	}
	return nil
}

// RedactURL removes the secret query parameters (Authorization) from a
// handshake URL so it can be logged. A URL that does not parse is returned as a
// fixed placeholder rather than echoed, so a secret cannot leak through a
// malformed input.
func RedactURL(raw string) string {
	const placeholder = "<invalid url>"
	u, err := url.Parse(raw)
	if err != nil {
		return placeholder
	}
	q := u.Query()
	if q.Has("Authorization") {
		q.Set("Authorization", "***")
	}
	u.RawQuery = q.Encode()
	if u.User != nil {
		u.User = url.User("***")
	}
	return u.String()
}

// frameJSON is the shape cmd/dgwprobe prints and writes.
type frameJSON struct {
	Type   string `json:"type"`
	RawB64 string `json:"raw_b64"`
	Size   int    `json:"size"`
}

// DescribeFrame renders a frame as the probe's line record.
func DescribeFrame(f Frame) ([]byte, error) {
	return json.Marshal(frameJSON{
		Type:   f.Type.String(),
		RawB64: base64.StdEncoding.EncodeToString(f.Raw),
		Size:   len(f.Raw),
	})
}
