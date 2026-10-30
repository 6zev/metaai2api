package updater

import (
	"context"
	"net/url"
	"testing"

	"github.com/me/metaai2api/internal/proxy"
)

// TestUsesProxyCurrent proves the updater dials through proxy.Current() when no
// client is injected: a published dead proxy makes the request fail, and
// republishing a direct transport makes the same call succeed. If the updater
// used http.DefaultTransport it would reach the stub in both cases.
func TestUsesProxyCurrent(t *testing.T) {
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v0.0.1",
		assetName: "app-linux-amd64",
		asset:     []byte("x"),
	})

	dead, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("parse dead proxy url: %v", err)
	}
	tr, err := proxy.Build(dead)
	if err != nil {
		t.Fatalf("build proxy transport: %v", err)
	}
	proxy.Provide(tr)
	t.Cleanup(func() { proxy.Provide(nil) })

	u := New(Config{
		Owner: "me", Repo: "metaai2api", AssetPattern: "app-{os}-{arch}",
		CurrentVersion: "v0.0.1", APIBase: s.srv.URL, GOOS: "linux", GOARCH: "amd64",
	})
	if u.cfg.HTTPClient != nil {
		t.Fatal("test expects no injected client")
	}
	if _, err := u.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded through a dead proxy: updater bypassed proxy.Current()")
	}

	direct, err := proxy.Build(nil)
	if err != nil {
		t.Fatalf("build direct transport: %v", err)
	}
	proxy.Provide(direct)
	if _, err := u.Check(context.Background()); err != nil {
		t.Fatalf("Check through direct transport: %v", err)
	}
}
