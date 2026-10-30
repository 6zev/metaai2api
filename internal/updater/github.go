package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/me/metaai2api/internal/proxy"
	"github.com/me/metaai2api/internal/version"
)

// Typed errors, all matchable with errors.Is.
var (
	// ErrAssetNotFound reports a release without an asset matching the
	// expanded asset_pattern.
	ErrAssetNotFound = errors.New("updater: no release asset matches the asset pattern")
	// ErrChecksumsUnavailable reports that checksums.txt could not be fetched
	// or that it has no entry for the selected asset.
	ErrChecksumsUnavailable = errors.New("updater: checksums.txt unavailable")
	// ErrChecksumMismatch reports downloaded bytes whose SHA256 differs from
	// checksums.txt.
	ErrChecksumMismatch = errors.New("updater: sha256 mismatch")
	// ErrNotConfigured reports an Updater without an owner, repo or asset
	// pattern.
	ErrNotConfigured = errors.New("updater: not configured")
)

const (
	// DefaultAPIBase is the GitHub Releases API root.
	DefaultAPIBase = "https://api.github.com"
	// checksumsAsset is the release asset that carries SHA256 sums.
	checksumsAsset = "checksums.txt"
	// downloadLimit caps an asset download at 200 MiB; a release binary far
	// below that, and an unbounded copy would fill the disk on a bad host.
	downloadLimit = 200 << 20
	// maxChecksumsBytes caps checksums.txt at 1 MiB.
	maxChecksumsBytes = 1 << 20
	requestTimeout    = 60 * time.Second
)

// Config configures an Updater. Zero values fall back to sensible defaults.
type Config struct {
	// Owner and Repo identify the GitHub repository. Required.
	Owner string
	Repo  string
	// AssetPattern selects the release asset; {os} and {arch} expand to GOOS
	// and GOARCH. Required.
	AssetPattern string
	// CurrentVersion is the running version. Empty reads version.Version.
	CurrentVersion string
	// APIBase overrides the GitHub API root. Empty uses DefaultAPIBase.
	APIBase string
	// TargetPath is the binary to replace. Empty uses os.Executable().
	TargetPath string
	// HTTPClient overrides the client. Nil builds one around proxy.Current().
	// Every production call routes through proxy.Current() one way or another.
	HTTPClient *http.Client
	// GOOS and GOARCH override the platform in the asset pattern. Empty uses
	// the running platform.
	GOOS   string
	GOARCH string
	// Now overrides the clock used for result timestamps. Optional.
	Now func() time.Time
}

// Result describes one Check.
type Result struct {
	Current   string
	Latest    string
	Version   string // the version now on disk, set when Applied
	AssetName string
	Available bool
	Applied   bool
	Bytes     int64
	SHA256    string
	CheckedAt time.Time
}

// Updater checks GitHub Releases and, when a newer verified asset exists,
// replaces the running binary.
type Updater struct {
	cfg Config

	mu      sync.Mutex
	last    Result
	lastAt  time.Time
	lastErr error
}

// New returns an Updater for cfg. It does not contact the network; a missing
// owner, repo or asset pattern only surfaces from Check.
func New(cfg Config) *Updater {
	if cfg.APIBase == "" {
		cfg.APIBase = DefaultAPIBase
	}
	if cfg.GOOS == "" {
		cfg.GOOS = runtime.GOOS
	}
	if cfg.GOARCH == "" {
		cfg.GOARCH = runtime.GOARCH
	}
	return &Updater{cfg: cfg}
}

// Last returns the most recent Check result, when it ran, and its error.
func (u *Updater) Last() (Result, time.Time, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.last, u.lastAt, u.lastErr
}

// release is the subset of the GitHub release payload the updater uses.
type release struct {
	TagName string  `json:"tag_name"`
	Assets  []asset `json:"assets"`
}

type asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// assetByName returns the asset called name.
func (r release) assetByName(name string) (asset, error) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, nil
		}
	}
	return asset{}, fmt.Errorf("%w: %q in release %s", ErrAssetNotFound, name, r.TagName)
}

// Check queries the latest release. When the release is newer than the running
// version it downloads the selected asset beside the target binary, verifies
// its SHA256 against checksums.txt and renames it over the target. Any failure
// before the final rename leaves the target untouched and removes the
// temporary file.
func (u *Updater) Check(ctx context.Context) (Result, error) {
	res, err := u.check(ctx)
	u.mu.Lock()
	u.last, u.lastErr = res, err
	u.lastAt = u.now()
	u.mu.Unlock()
	return res, err
}

func (u *Updater) check(ctx context.Context) (Result, error) {
	if strings.TrimSpace(u.cfg.Owner) == "" || strings.TrimSpace(u.cfg.Repo) == "" {
		return Result{}, fmt.Errorf("%w: owner and repo are required", ErrNotConfigured)
	}
	if strings.TrimSpace(u.cfg.AssetPattern) == "" {
		return Result{}, fmt.Errorf("%w: asset_pattern is required", ErrNotConfigured)
	}

	cur := u.currentVersion()
	res := Result{Current: cur, CheckedAt: u.now()}

	rel, err := u.latestRelease(ctx)
	if err != nil {
		return res, err
	}
	res.Latest = rel.TagName

	if !isNewer(rel.TagName, cur) {
		return res, nil
	}
	res.Available = true

	target := u.cfg.TargetPath
	if target == "" {
		target, err = os.Executable()
		if err != nil {
			return res, fmt.Errorf("updater: locate executable: %w", err)
		}
	}
	if st, serr := os.Lstat(target); serr != nil {
		return res, fmt.Errorf("updater: stat target: %w", serr)
	} else if !st.Mode().IsRegular() {
		return res, fmt.Errorf("%w: %s is not a regular file", ErrTargetNotRegular, target)
	} else if st.Mode().Perm()&0o111 == 0 {
		return res, fmt.Errorf("%w: %s is not executable", ErrTargetNotRegular, target)
	}

	want := expandAssetPattern(u.cfg.AssetPattern, u.cfg.GOOS, u.cfg.GOARCH)
	res.AssetName = want
	a, err := rel.assetByName(want)
	if err != nil {
		return res, err
	}

	sum, err := u.fetchChecksum(ctx, rel, want)
	if err != nil {
		return res, err
	}

	staged, n, got, err := u.downloadVerified(ctx, a.BrowserDownloadURL, sum, target)
	if err != nil {
		return res, err
	}
	res.Bytes = n
	res.SHA256 = hex.EncodeToString(got)

	if err := u.install(staged, target); err != nil {
		return res, err
	}
	res.Applied = true
	res.Version = rel.TagName

	// Record the applied version so a second Check in this process does not
	// download the same release again.
	u.mu.Lock()
	u.cfg.CurrentVersion = rel.TagName
	u.mu.Unlock()
	return res, nil
}

// install performs the final swap. staged was written and fsynced beside
// target, so the rename cannot leave target partial. On Windows the running
// image cannot be overwritten; it is renamed aside and the new binary renamed
// in. The staging file is removed on every error path.
func (u *Updater) install(staged, target string) error {
	err := renameInto(staged, target)
	if err == nil {
		return nil
	}
	if !useAsideFallback() {
		// On POSIX a failed rename is a real failure; failing closed here is
		// what keeps the old binary in place.
		os.Remove(staged)
		return err
	}
	aside := target + asideSuffix
	if asideErr := replaceAside(staged, target, aside); asideErr != nil {
		os.Remove(staged)
		return fmt.Errorf("%w (direct rename: %v)", asideErr, err)
	}
	return nil
}

func (u *Updater) now() time.Time {
	if u.cfg.Now != nil {
		return u.cfg.Now()
	}
	return time.Now()
}

func (u *Updater) currentVersion() string {
	if u.cfg.CurrentVersion != "" {
		return u.cfg.CurrentVersion
	}
	return version.Version
}

// client returns the HTTP client every call uses. A caller-supplied client
// wins so tests stay off the network; otherwise the transport currently
// published by proxy is used, never http.DefaultTransport.
func (u *Updater) client() *http.Client {
	if u.cfg.HTTPClient != nil {
		return u.cfg.HTTPClient
	}
	return &http.Client{Transport: proxy.Current(), Timeout: requestTimeout}
}

func (u *Updater) latestRelease(ctx context.Context) (release, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/releases/latest", strings.TrimRight(u.cfg.APIBase, "/"), u.cfg.Owner, u.cfg.Repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return release{}, fmt.Errorf("updater: build release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "metaai2api-updater/"+u.currentVersion())

	resp, err := u.client().Do(req)
	if err != nil {
		return release{}, fmt.Errorf("updater: query latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return release{}, fmt.Errorf("updater: latest release: unexpected status %s", resp.Status)
	}
	var rel release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return release{}, fmt.Errorf("updater: decode latest release: %w", err)
	}
	if strings.TrimSpace(rel.TagName) == "" {
		return release{}, errors.New("updater: latest release has no tag_name")
	}
	return rel, nil
}

// fetchChecksum GETs checksums.txt for the release and returns the SHA256 for
// assetName. Absence of the asset, a non-200 response or a missing entry is
// ErrChecksumsUnavailable, which fails the update closed.
func (u *Updater) fetchChecksum(ctx context.Context, rel release, assetName string) ([]byte, error) {
	a, err := rel.assetByName(checksumsAsset)
	if err != nil {
		return nil, fmt.Errorf("%w: release %s has no %s", ErrChecksumsUnavailable, rel.TagName, checksumsAsset)
	}
	body, err := u.get(ctx, a.BrowserDownloadURL, maxChecksumsBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrChecksumsUnavailable, err)
	}
	sum, ok := checksumFor(strings.NewReader(string(body)), assetName)
	if !ok {
		return nil, fmt.Errorf("%w: no entry for %q", ErrChecksumsUnavailable, assetName)
	}
	return sum, nil
}

// downloadVerified streams the asset into a staging file beside target while
// hashing it, aborts without touching target when the hash differs, and
// returns the staging file's path, size and digest on success. The caller owns
// the staging file: either rename it in or remove it.
func (u *Updater) downloadVerified(ctx context.Context, url string, want []byte, target string) (string, int64, []byte, error) {
	body, err := u.open(ctx, url)
	if err != nil {
		return "", 0, nil, err
	}
	defer body.Close()

	hasher := sha256.New()
	limited := io.LimitReader(body, downloadLimit+1)
	// Hash while writing, without buffering the whole asset in memory.
	staged, err := createStaging(target, io.TeeReader(limited, hasher))
	if err != nil {
		return "", 0, nil, err
	}
	n, err := countStaged(staged)
	if err != nil {
		os.Remove(staged)
		return "", 0, nil, err
	}
	if n > downloadLimit {
		os.Remove(staged)
		return "", 0, nil, fmt.Errorf("updater: asset exceeds %d bytes", int64(downloadLimit))
	}
	got := hasher.Sum(nil)
	if !equalSum(got, want) {
		os.Remove(staged)
		return "", 0, nil, fmt.Errorf("%w: %s: got %s want %s", ErrChecksumMismatch, filepath.Base(url), hex.EncodeToString(got), hex.EncodeToString(want))
	}
	return staged, n, got, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	body, err := u.open(ctx, url)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	return io.ReadAll(io.LimitReader(body, limit+1))
}

func (u *Updater) open(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("updater: build download request: %w", err)
	}
	req.Header.Set("User-Agent", "metaai2api-updater/"+u.currentVersion())
	resp, err := u.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("updater: download: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("updater: download: unexpected status %s", resp.Status)
	}
	return resp.Body, nil
}

// expandAssetPattern substitutes {os} and {arch}.
func expandAssetPattern(pattern, goos, goarch string) string {
	r := strings.NewReplacer("{os}", goos, "{arch}", goarch)
	return r.Replace(pattern)
}

// checksumFor parses "sha256  name" lines (sha256sum and shasum formats, with
// or without the binary-mode "*") and returns the sum for name. Malformed
// lines are skipped: they can only make a lookup fail, never fake a match.
func checksumFor(r io.Reader, name string) ([]byte, bool) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sumHex, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		fileField := strings.TrimSpace(rest)
		fileField = strings.TrimPrefix(fileField, "*")
		if fileField != name {
			continue
		}
		sum, err := hex.DecodeString(sumHex)
		if err != nil || len(sum) != sha256.Size {
			continue
		}
		return sum, true
	}
	return nil, false
}

func equalSum(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	// crypto/subtle would be premature here: both operands are local digests,
	// not secret-dependent comparisons.
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// isNewer reports whether latest is a version after current. An unparseable
// current ("dev", "unknown") never blocks an update; equal or older tags do.
func isNewer(latest, current string) bool {
	lt, lok := parseVersion(latest)
	ct, cok := parseVersion(current)
	if !lok {
		return false
	}
	if !cok {
		return true
	}
	for i := 0; i < 3; i++ {
		if lt[i] != ct[i] {
			return lt[i] > ct[i]
		}
	}
	return false
}

// parseVersion turns "v1.2.3", "1.2.3-rc1", "v1.2" into three numeric parts.
func parseVersion(s string) ([3]int, bool) {
	var out [3]int
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "v"), "V")
	if s == "" {
		return out, false
	}
	// Drop any pre-release or build metadata.
	if i := strings.IndexAny(s, "-+"); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 3 {
		return out, false
	}
	for i, p := range parts {
		if p == "" {
			return out, false
		}
		n := 0
		for _, c := range p {
			if c < '0' || c > '9' {
				return out, false
			}
			n = n*10 + int(c-'0')
		}
		out[i] = n
	}
	return out, true
}
