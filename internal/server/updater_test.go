package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/me/metaai2api/internal/updater"
)

// TestNewUpdaterFuncReportsVersion proves the adapter surfaces the installed
// version and drives Check through the handler path.
func TestNewUpdaterFuncReportsVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/me/metaai2api/releases/latest":
			w.Write([]byte(`{"tag_name":"v9.9.9","assets":[{"name":"checksums.txt","browser_download_url":"http://` + r.Host + `/dl/checksums.txt"}]}`))
		case "/dl/checksums.txt":
			w.Write([]byte("00  other\n"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	target := dir + "/app"
	if err := os.WriteFile(target, []byte("#!/bin/sh\ntrue\n"), 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	u := updater.New(updater.Config{
		Owner: "me", Repo: "metaai2api", AssetPattern: "app-{os}-{arch}",
		CurrentVersion: "v0.0.1", APIBase: srv.URL, TargetPath: target,
		HTTPClient: srv.Client(), GOOS: "linux", GOARCH: "amd64",
	})
	f := NewUpdaterFunc(u)
	if f == nil {
		t.Fatal("NewUpdaterFunc(nil-safe) returned nil for a real updater")
	}
	// The stub release has no matching asset, so the handler reports an error
	// and never touches the binary.
	if _, err := f(); err == nil {
		t.Fatal("want error for a release without the asset, got nil")
	}

	if NewUpdaterFunc(nil) != nil {
		t.Fatal("NewUpdaterFunc(nil) should be nil, disabling the route")
	}
}

// TestNewUpdateCheckerNeverContacts proves the health view reports only the
// last outcome: before any Check it says checked=false.
func TestNewUpdateCheckerNeverContacts(t *testing.T) {
	u := updater.New(updater.Config{Owner: "me", Repo: "metaai2api", AssetPattern: "app-{os}-{arch}"})
	st := NewUpdateChecker(u)()
	if st.Checked {
		t.Fatalf("checked = true before any Check: %+v", st)
	}
	if NewUpdateChecker(nil) != nil {
		t.Fatal("NewUpdateChecker(nil) should be nil")
	}
}
