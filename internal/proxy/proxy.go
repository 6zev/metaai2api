// Package proxy builds the outbound transport used for every meta.ai call.
//
// Both the GraphQL client and the DGW WebSocket dial through the transport
// published here, so a configuration reload swaps the proxy atomically and
// neither path can accidentally bypass it.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	xproxy "golang.org/x/net/proxy"
)

const (
	defaultSocks5Port = "1080"
	dialTimeout       = 30 * time.Second
)

// Build returns a fresh transport that routes every request through u.
//
// A nil or empty u yields a direct transport with no proxy. Supported schemes
// are http, https, socks5 and socks5h; socks5h resolves names at the proxy.
// Each call returns a new transport so callers can publish it with Provide
// without mutating a transport others are still using.
func Build(u *url.URL) (*http.Transport, error) {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   dialTimeout,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}

	if u == nil || (u.Scheme == "" && u.Host == "") {
		return tr, nil
	}
	if u.Host == "" {
		return nil, fmt.Errorf("proxy: %q has no host", u.Redacted())
	}

	switch u.Scheme {
	case "http", "https":
		pu := *u
		tr.Proxy = http.ProxyURL(&pu)
	case "socks5", "socks5h":
		d, err := socksDialer(u)
		if err != nil {
			return nil, err
		}
		tr.DialContext = d.DialContext
	case "":
		return nil, errors.New("proxy: missing scheme")
	default:
		return nil, fmt.Errorf("proxy: unsupported scheme %q", u.Scheme)
	}
	return tr, nil
}

type contextDialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// socksConnDialer is what xproxy.SOCKS5 returns; it satisfies both proxy.Dialer
// (Dial) and contextDialer (DialContext).
type socksConnDialer interface {
	xproxy.Dialer
	contextDialer
}

// Dialer returns a proxy.Dialer that sends every connection through u.
//
// The WebSocket path consumes this directly, so it must never fall back to a
// direct connection when a proxy is configured. A nil or empty u returns a
// direct dialer.
func Dialer(u *url.URL) (xproxy.Dialer, error) {
	if u == nil || (u.Scheme == "" && u.Host == "") {
		return xproxy.Direct, nil
	}
	if u.Scheme != "socks5" && u.Scheme != "socks5h" {
		return nil, fmt.Errorf("proxy: scheme %q cannot back a raw dialer; use http/https via Build", u.Scheme)
	}
	return socksDialer(u)
}

func socksDialer(u *url.URL) (socksConnDialer, error) {
	if u.Host == "" {
		return nil, fmt.Errorf("proxy: %q has no host", u.Redacted())
	}
	addr := u.Host
	if u.Port() == "" {
		addr = net.JoinHostPort(u.Hostname(), defaultSocks5Port)
	}
	forward := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	d, err := xproxy.SOCKS5("tcp", addr, socksAuth(u), forward)
	if err != nil {
		return nil, fmt.Errorf("proxy: socks5 %s: %w", addr, err)
	}
	cd, ok := d.(socksConnDialer)
	if !ok {
		return nil, fmt.Errorf("proxy: socks5 dialer for %s lacks DialContext", addr)
	}
	return cd, nil
}

func socksAuth(u *url.URL) *xproxy.Auth {
	if u.User == nil {
		return nil
	}
	name := u.User.Username()
	if name == "" {
		return nil
	}
	auth := &xproxy.Auth{User: name}
	auth.Password, _ = u.User.Password()
	return auth
}

var (
	mu      sync.RWMutex
	current = newDefault()
)

// newDefault returns the direct transport used before any Provide call.
func newDefault() *http.Transport {
	tr, err := Build(nil)
	if err != nil {
		// Build(nil) has no failure mode; a panic here means Build regressed.
		panic(fmt.Sprintf("proxy: default transport: %v", err))
	}
	return tr
}

// Provide atomically publishes t as the transport every outbound call uses.
// A nil t resets to a fresh direct transport so Current never returns nil.
// The previously published transport is left untouched.
func Provide(t *http.Transport) {
	if t == nil {
		t = newDefault()
	}
	mu.Lock()
	current = t
	mu.Unlock()
}

// Current returns the transport to dial with. Callers must not mutate it.
func Current() *http.Transport {
	mu.RLock()
	t := current
	mu.RUnlock()
	return t
}
