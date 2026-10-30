package updater

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubGitHub is an httptest stand-in for the GitHub Releases API and the asset
// hosts. No test here touches the network.
type stubGitHub struct {
	owner, repo string
	version     string
	assetName   string
	asset       []byte
	checksums   string // body of checksums.txt; empty means 404
	omitAsset   bool
	checksum404 bool

	srv *httptest.Server
}

func newStubGitHub(t *testing.T, s *stubGitHub) *stubGitHub {
	t.Helper()
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/"+s.owner+"/"+s.repo+"/releases/latest":
			assets := []map[string]any{}
			if !s.omitAsset {
				assets = append(assets, map[string]any{
					"name":                 s.assetName,
					"browser_download_url": s.srv.URL + "/dl/" + s.assetName,
				})
			}
			assets = append(assets, map[string]any{
				"name":                 "checksums.txt",
				"browser_download_url": s.srv.URL + "/dl/checksums.txt",
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"tag_name": s.version,
				"assets":   assets,
			})
		case r.URL.Path == "/dl/checksums.txt":
			if s.checksum404 || s.checksums == "" {
				http.NotFound(w, r)
				return
			}
			io.WriteString(w, s.checksums)
		case r.URL.Path == "/dl/"+s.assetName:
			w.Write(s.asset)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// checksumLine renders a standard "sha256  name" checksums.txt line.
func checksumLine(sum []byte, name string) string {
	return hex.EncodeToString(sum) + "  " + name + "\n"
}

// testUpdater points an Updater at the stub and a fake target binary in a temp
// dir. The target is executable so replace() accepts it.
func testUpdater(t *testing.T, s *stubGitHub, old []byte) (*Updater, string, string) {
	t.Helper()
	dir := t.TempDir()
	target := filepath.Join(dir, "app")
	if err := os.WriteFile(target, old, 0o755); err != nil {
		t.Fatalf("write target: %v", err)
	}
	u := New(Config{
		Owner:          s.owner,
		Repo:           s.repo,
		AssetPattern:   "app-{os}-{arch}",
		CurrentVersion: "v0.0.1",
		APIBase:        s.srv.URL,
		TargetPath:     target,
		HTTPClient:     s.srv.Client(),
		GOOS:           "linux",
		GOARCH:         "amd64",
	})
	return u, target, dir
}

func TestChecksumMismatchKeepsOldBinary(t *testing.T) {
	old := []byte("old binary bytes")
	newBin := []byte("brand new binary bytes")
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v1.2.3",
		assetName: "app-linux-amd64",
		asset:     newBin,
		// A well-formed but wrong hash: the download must never be installed.
		checksums: checksumLine(sha256Sum([]byte("something else")), "app-linux-amd64"),
	})
	u, target, dir := testUpdater(t, s, old)

	res, err := u.Check(context.Background())
	if err == nil {
		t.Fatalf("Check: want checksum error, got nil (result %+v)", res)
	}
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("err = %v, want ErrChecksumMismatch", err)
	}

	got, readErr := os.ReadFile(target)
	if readErr != nil {
		t.Fatalf("read target: %v", readErr)
	}
	if !bytes.Equal(got, old) {
		t.Fatalf("old binary changed: got %q, want %q", got, old)
	}
	assertNoTempLeftovers(t, dir, "app")
}

func TestChecksumsUnavailableFailsClosed(t *testing.T) {
	old := []byte("old binary bytes")
	cases := map[string]func(*stubGitHub){
		"checksums.txt 404": func(s *stubGitHub) { s.checksum404 = true },
		"no matching entry": func(s *stubGitHub) {
			s.checksums = checksumLine(sha256Sum([]byte("x")), "app-darwin-arm64")
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubGitHub(t, &stubGitHub{
				owner: "me", repo: "metaai2api",
				version:   "v1.2.3",
				assetName: "app-linux-amd64",
				asset:     []byte("brand new binary bytes"),
			})
			mutate(s)
			u, target, dir := testUpdater(t, s, old)

			res, err := u.Check(context.Background())
			if err == nil {
				t.Fatalf("Check: want error, got nil (result %+v)", res)
			}
			if !errors.Is(err, ErrChecksumsUnavailable) {
				t.Fatalf("err = %v, want ErrChecksumsUnavailable", err)
			}
			got, readErr := os.ReadFile(target)
			if readErr != nil {
				t.Fatalf("read target: %v", readErr)
			}
			if !bytes.Equal(got, old) {
				t.Fatalf("old binary changed: got %q, want %q", got, old)
			}
			assertNoTempLeftovers(t, dir, "app")
		})
	}
}

func TestNoTempLeftoversOnFailure(t *testing.T) {
	old := []byte("old binary bytes")
	cases := map[string]struct {
		mutate func(*stubGitHub)
		errIs  error
	}{
		"checksum mismatch": {
			mutate: func(s *stubGitHub) {
				s.checksums = checksumLine(sha256Sum([]byte("wrong")), "app-linux-amd64")
			},
			errIs: ErrChecksumMismatch,
		},
		"checksums missing": {
			mutate: func(s *stubGitHub) { s.checksum404 = true },
			errIs:  ErrChecksumsUnavailable,
		},
		"asset missing from release": {
			mutate: func(s *stubGitHub) { s.omitAsset = true },
			errIs:  ErrAssetNotFound,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			s := newStubGitHub(t, &stubGitHub{
				owner: "me", repo: "metaai2api",
				version:   "v1.2.3",
				assetName: "app-linux-amd64",
				asset:     []byte("brand new binary bytes"),
			})
			tc.mutate(s)
			u, target, dir := testUpdater(t, s, old)

			if _, err := u.Check(context.Background()); !errors.Is(err, tc.errIs) {
				t.Fatalf("err = %v, want %v", err, tc.errIs)
			}
			if got, _ := os.ReadFile(target); !bytes.Equal(got, old) {
				t.Fatalf("old binary changed: got %q", got)
			}
			assertNoTempLeftovers(t, dir, "app")
		})
	}
}

func TestCheckUpToDateDoesNothing(t *testing.T) {
	old := []byte("old binary bytes")
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v0.0.1",
		assetName: "app-linux-amd64",
		asset:     []byte("brand new binary bytes"),
	})
	u, target, dir := testUpdater(t, s, old)

	res, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if res.Applied || res.Available {
		t.Fatalf("up-to-date result = %+v, want not available and not applied", res)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, old) {
		t.Fatalf("old binary changed: got %q", got)
	}
	assertNoTempLeftovers(t, dir, "app")
}

func TestCheckInstallsVerifiedAsset(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-over-running-binary semantics differ; covered by replace tests")
	}
	old := []byte("#!/bin/sh\necho old\n")
	newBin := []byte("#!/bin/sh\necho new\n")
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v1.2.3",
		assetName: "app-linux-amd64",
		asset:     newBin,
		checksums: checksumLine(sha256Sum(newBin), "app-linux-amd64"),
	})
	u, target, dir := testUpdater(t, s, old)

	res, err := u.Check(context.Background())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !res.Applied || !res.Available {
		t.Fatalf("result = %+v, want available and applied", res)
	}
	if res.Version != "v1.2.3" {
		t.Fatalf("version = %q, want v1.2.3", res.Version)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, newBin) {
		t.Fatalf("target = %q, want new binary", got)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary is not executable: %v", info.Mode())
	}
	if runtime.GOOS != "windows" {
		out, runErr := exec.Command(target).CombinedOutput()
		if runErr != nil {
			t.Fatalf("run installed binary: %v (%s)", runErr, out)
		}
		if !strings.Contains(string(out), "new") {
			t.Fatalf("installed binary output = %q, want it to contain %q", out, "new")
		}
	}
	assertNoTempLeftovers(t, dir, "app")
}

func TestAssetPatternExpansion(t *testing.T) {
	cases := []struct {
		pattern, goos, goarch, want string
	}{
		{"app-{os}-{arch}", "linux", "amd64", "app-linux-amd64"},
		{"app_{os}_{arch}.exe", "windows", "arm64", "app_windows_arm64.exe"},
		{"app-{os}-{arch}-{os}", "darwin", "arm64", "app-darwin-arm64-darwin"},
		{"app-static", "linux", "amd64", "app-static"},
	}
	for _, tc := range cases {
		if got := expandAssetPattern(tc.pattern, tc.goos, tc.goarch); got != tc.want {
			t.Errorf("expandAssetPattern(%q, %q, %q) = %q, want %q", tc.pattern, tc.goos, tc.goarch, got, tc.want)
		}
	}
}

func TestChecksumParsing(t *testing.T) {
	sum := sha256Sum([]byte("payload"))
	sumHex := hex.EncodeToString(sum)
	text := "" +
		"# generated by goreleaser\n" +
		sumHex + "  app-linux-amd64\n" +
		sumHex + " *app-darwin-arm64\n" +
		"\n" +
		"deadbeef  short-sum\n" +
		"not-a-hash  app-linux-amd64\n"

	got, ok := checksumFor(strings.NewReader(text), "app-linux-amd64")
	if !ok {
		t.Fatal("checksumFor: entry not found")
	}
	if !bytes.Equal(got, sum) {
		t.Fatalf("checksum = %x, want %x", got, sum)
	}
	if _, ok := checksumFor(strings.NewReader(text), "app-windows-amd64"); ok {
		t.Fatal("checksumFor: found an entry that is not in the file")
	}
}

func TestAssetSelectionMatchesExpandedName(t *testing.T) {
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v0.0.1",
		assetName: "app-linux-amd64",
		asset:     []byte("x"),
	})
	u, _, _ := testUpdater(t, s, []byte("old"))
	stub, err := u.latestRelease(context.Background())
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	name := expandAssetPattern(u.cfg.AssetPattern, u.cfg.GOOS, u.cfg.GOARCH)
	asset, err := stub.assetByName(name)
	if err != nil {
		t.Fatalf("assetByName(%q): %v", name, err)
	}
	if asset.Name != "app-linux-amd64" {
		t.Fatalf("selected asset = %q", asset.Name)
	}
	if _, err := stub.assetByName("app-windows-amd64"); !errors.Is(err, ErrAssetNotFound) {
		t.Fatalf("assetByName mismatch err = %v, want ErrAssetNotFound", err)
	}
}

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

// assertNoTempLeftovers fails when dir holds anything besides the target and a
// legitimately retained aside copy next to it.
func assertNoTempLeftovers(t *testing.T, dir, targetName string) {
	t.Helper()
	for _, name := range dirEntries(t, dir) {
		if name == targetName {
			continue
		}
		if name == targetName+".old" {
			// An aside is only legitimate on Windows, and only after a
			// successful replace; failures must not create one.
			t.Fatalf("unexpected aside leftover after failure: %s", name)
		}
		t.Fatalf("temp file left behind after failure: %s", name)
	}
}

// TestCheckRefusesNonExecutableTarget proves Check never installs over a target
// that replace() would reject, and that nothing is left behind.
func TestCheckRefusesNonExecutableTarget(t *testing.T) {
	old := []byte("old binary bytes")
	newBin := []byte("brand new binary bytes")
	s := newStubGitHub(t, &stubGitHub{
		owner: "me", repo: "metaai2api",
		version:   "v1.2.3",
		assetName: "app-linux-amd64",
		asset:     newBin,
		checksums: checksumLine(sha256Sum(newBin), "app-linux-amd64"),
	})
	u, target, dir := testUpdater(t, s, old)
	if err := os.Chmod(target, 0o644); err != nil {
		t.Fatalf("chmod target: %v", err)
	}

	if _, err := u.Check(context.Background()); !errors.Is(err, ErrTargetNotRegular) {
		t.Fatalf("err = %v, want ErrTargetNotRegular", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read target: %v", err)
	}
	if !bytes.Equal(got, old) {
		t.Fatalf("target changed: got %q", got)
	}
	assertNoTempLeftovers(t, dir, "app")
}
