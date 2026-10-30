package proxy

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSOCKS5 is a minimal RFC 1928 server: no-auth greeting, CONNECT to an
// IPv4/domain target, then a raw byte pipe. It records the targets it was
// asked to connect to so tests can prove traffic traversed it.
type fakeSOCKS5 struct {
	ln net.Listener
	mu sync.Mutex
	// targets records the destination address of every CONNECT request.
	targets []string
	// replies dials the recorded target for real; when nil it echoes.
	replies bool
}

func newFakeSOCKS5(t *testing.T) *fakeSOCKS5 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSOCKS5{ln: ln, replies: true}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeSOCKS5) addr() string { return f.ln.Addr().String() }

func (f *fakeSOCKS5) url() *url.URL {
	return &url.URL{Scheme: "socks5", Host: f.addr()}
}

func (f *fakeSOCKS5) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.targets...)
}

func (f *fakeSOCKS5) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(c)
	}
}

func (f *fakeSOCKS5) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))

	// Greeting: VER NMETHODS METHODS...
	hdr := make([]byte, 2)
	if _, err := io.ReadFull(c, hdr); err != nil || hdr[0] != 5 {
		return
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(c, methods); err != nil {
		return
	}
	if _, err := c.Write([]byte{5, 0}); err != nil { // no auth
		return
	}

	// Request: VER CMD RSV ATYP ADDR PORT
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil || req[0] != 5 || req[1] != 1 {
		return
	}
	var host string
	switch req[3] {
	case 1:
		b := make([]byte, 4)
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	case 3:
		var l [1]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		b := make([]byte, int(l[0]))
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	default:
		return
	}
	var p [2]byte
	if _, err := io.ReadFull(c, p[:]); err != nil {
		return
	}
	target := net.JoinHostPort(host, itoa(int(p[0])<<8|int(p[1])))

	f.mu.Lock()
	f.targets = append(f.targets, target)
	f.mu.Unlock()

	// Success reply with a bound address of 0.0.0.0:0.
	if _, err := c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		return
	}

	if !f.replies {
		return
	}
	upstream, err := net.Dial("tcp", target)
	if err != nil {
		return
	}
	defer upstream.Close()
	go io.Copy(upstream, c)
	io.Copy(c, upstream)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func TestSocks5Dial(t *testing.T) {
	echo := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "through-socks")
	}))
	defer echo.Close()
	echoURL, _ := url.Parse(echo.URL)

	socks := newFakeSOCKS5(t)

	tr, err := Build(socks.url())
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(tr.CloseIdleConnections)

	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(echo.URL)
	if err != nil {
		t.Fatalf("GET via socks5: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "through-socks" {
		t.Fatalf("body = %q", body)
	}

	seen := socks.seen()
	if len(seen) == 0 {
		t.Fatal("socks5 proxy saw no CONNECT; traffic did not traverse it")
	}
	if seen[0] != echoURL.Host {
		t.Fatalf("proxy target = %q, want %q", seen[0], echoURL.Host)
	}
}

func TestSocks5hResolvesAtProxy(t *testing.T) {
	socks := newFakeSOCKS5(t)
	socks.replies = false // we only inspect the requested target

	u := socks.url()
	u.Scheme = "socks5h"
	tr, err := Build(u)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(tr.CloseIdleConnections)

	client := &http.Client{Transport: tr, Timeout: 2 * time.Second}
	// The name does not resolve locally; socks5h must hand it to the proxy.
	client.Get("http://example.invalid:8081/")

	seen := socks.seen()
	if len(seen) == 0 {
		t.Fatal("proxy saw no CONNECT")
	}
	if seen[0] != "example.invalid:8081" {
		t.Fatalf("socks5h target = %q, want hostname sent unresolved", seen[0])
	}
}

func TestDialerSocks5(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			io.WriteString(c, "raw")
			c.Close()
		}
	}()

	socks := newFakeSOCKS5(t)
	d, err := Dialer(socks.url())
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	conn, err := d.Dial("tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 3)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(buf) != "raw" {
		t.Fatalf("read %q", buf)
	}
	if seen := socks.seen(); len(seen) == 0 || seen[0] != echo.Addr().String() {
		t.Fatalf("socks5 saw %v, want %q", seen, echo.Addr().String())
	}
}

func TestBuildHTTPConnect(t *testing.T) {
	// Plain targets are proxied in absolute-form; CONNECT is used for TLS
	// (https) targets. Test the CONNECT tunnel with an httptest TLS server.
	target := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "via-connect")
	}))
	defer target.Close()

	var mu sync.Mutex
	var connects []string
	// A forwarding proxy that implements HTTP CONNECT tunnelling only.
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			http.Error(w, "connect only", http.StatusMethodNotAllowed)
			return
		}
		mu.Lock()
		connects = append(connects, r.Host)
		mu.Unlock()
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer upstream.Close()
		hc, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "no hijack", http.StatusInternalServerError)
			return
		}
		client, _, err := hc.Hijack()
		if err != nil {
			return
		}
		defer client.Close()
		io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{}, 2)
		go func() { io.Copy(upstream, client); done <- struct{}{} }()
		go func() { io.Copy(client, upstream); done <- struct{}{} }()
		<-done
	}))
	defer fwd.Close()
	fwdURL, _ := url.Parse(fwd.URL)

	targetURL, _ := url.Parse(target.URL)

	tr, err := Build(fwdURL)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(tr.CloseIdleConnections)
	tr.TLSClientConfig = target.Client().Transport.(*http.Transport).TLSClientConfig.Clone()

	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(target.URL)
	if err != nil {
		t.Fatalf("GET via http proxy: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "via-connect" {
		t.Fatalf("body = %q", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connects) != 1 || connects[0] != targetURL.Host {
		t.Fatalf("CONNECT targets = %v, want [%s]", connects, targetURL.Host)
	}
}

// TestBuildHTTPProxyAbsoluteForm proves plain http targets are forwarded to the
// proxy in absolute-form rather than dialed directly (no proxy bypass).
func TestBuildHTTPProxyAbsoluteForm(t *testing.T) {
	var mu sync.Mutex
	var sawProxy bool
	fwd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sawProxy = true
		mu.Unlock()
		io.WriteString(w, "abs:"+r.URL.String())
	}))
	defer fwd.Close()
	fwdURL, _ := url.Parse(fwd.URL)

	tr, err := Build(fwdURL)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(tr.CloseIdleConnections)
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get("http://meta.example.test/x?y=1")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "meta.example.test/x?y=1") {
		t.Fatalf("body = %q; proxy did not see absolute URL", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if !sawProxy {
		t.Fatal("proxy not traversed")
	}
}

func TestBuildDirect(t *testing.T) {
	for _, u := range []*url.URL{nil, {}} {
		tr, err := Build(u)
		if err != nil {
			t.Fatalf("Build(%v): %v", u, err)
		}
		if tr.Proxy != nil {
			if p, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "meta.ai"}}); err != nil || p != nil {
				t.Fatalf("Build(%v): Proxy = %v, %v; want nil", u, p, err)
			}
		}
		if d, err := Dialer(u); err != nil || d == nil {
			t.Fatalf("Dialer(%v): %v, %v", u, d, err)
		}
	}
}

func TestBuildRejectsBadScheme(t *testing.T) {
	bad := []string{"ftp://127.0.0.1:1", "://bad", "socks5://"}
	for _, raw := range bad {
		u, _ := url.Parse(raw)
		if u == nil {
			continue
		}
		if _, err := Build(u); err == nil {
			t.Fatalf("Build(%q) succeeded, want error", raw)
		}
		if _, err := Dialer(u); err == nil {
			t.Fatalf("Dialer(%q) succeeded, want error", raw)
		}
	}
}

func TestBuildSocks5DefaultsPort(t *testing.T) {
	u, _ := url.Parse("socks5://127.0.0.1")
	// Portless proxy: Build must not error, and must target the default 1080.
	tr, err := Build(u)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tr == nil {
		t.Fatal("nil transport")
	}
}

func TestProvideCurrent(t *testing.T) {
	base, err := Build(nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	before := Current()
	Provide(base)
	if got := Current(); got != base {
		t.Fatalf("Current = %p, want %p", got, base)
	}
	// A nil Provide resets to a fresh default transport rather than storing nil.
	Provide(nil)
	if Current() == nil {
		t.Fatal("Current = nil after Provide(nil)")
	}
	if Current() == base {
		t.Fatal("Current still the old transport after reset")
	}
	Provide(before)
}

func TestConcurrentProvideCurrent(t *testing.T) {
	pair, err := Build(nil)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	t.Cleanup(func() { Provide(nil) })

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					Provide(pair)
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if tr := Current(); tr != nil {
						// Touch a field to ensure the published value is usable.
						_ = tr.TLSHandshakeTimeout
					}
				}
			}
		}()
	}

	time.Sleep(100 * time.Millisecond)
	close(stop)
	wg.Wait()

	if Current() == nil {
		t.Fatal("Current = nil")
	}
}

// TestDialerCtxCancellation proves the Dialer honors a cancelled context so the
// WebSocket path can abort an in-flight dial.
func TestDialerCtxCancellation(t *testing.T) {
	socks := newFakeSOCKS5(t)
	socks.replies = false
	d, err := Dialer(socks.url())
	if err != nil {
		t.Fatalf("Dialer: %v", err)
	}
	cd, ok := d.(interface {
		DialContext(context.Context, string, string) (net.Conn, error)
	})
	if !ok {
		t.Fatalf("Dialer %T does not implement DialContext", d)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := cd.DialContext(ctx, "tcp", "example.invalid:80"); !errors.Is(err, context.Canceled) {
		t.Fatalf("DialContext err = %v, want context.Canceled", err)
	}
}

func TestBuildTransportIsFresh(t *testing.T) {
	u, _ := url.Parse("socks5://127.0.0.1:1080")
	a, err := Build(u)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	b, err := Build(u)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if a == b {
		t.Fatal("Build returned the same transport twice; reload would mutate shared state")
	}
}

func TestProxyURLStringSanity(t *testing.T) {
	u, _ := url.Parse("http://user:pass@127.0.0.1:9050")
	tr, err := Build(u)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("Build(http proxy) left Proxy nil")
	}
	got, err := tr.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "meta.ai"}})
	if err != nil {
		t.Fatalf("Proxy: %v", err)
	}
	if got == nil || !strings.Contains(got.String(), "127.0.0.1:9050") {
		t.Fatalf("Proxy = %v", got)
	}
	if got.User == nil {
		t.Fatal("proxy credentials dropped")
	}
	if pw, _ := got.User.Password(); pw != "pass" {
		t.Fatalf("proxy password = %q", pw)
	}
}
