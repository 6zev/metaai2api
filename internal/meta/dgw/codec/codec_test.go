package codec_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/codec"
)

func TestNewWithoutWasmReportsUnavailable(t *testing.T) {
	c, err := codec.New()
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("New() error = %v, want backend.ErrUnavailable", err)
	}
	if c != nil {
		t.Fatalf("New() returned %#v, want nil codec on error", c)
	}
}

func TestAssetPathMissing(t *testing.T) {
	path, err := codec.AssetPath()
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("AssetPath() error = %v, want backend.ErrUnavailable", err)
	}
	if path != "" {
		t.Fatalf("AssetPath() = %q, want empty", path)
	}
}

func TestAssetPathHonoursEnvironmentOnlyInTaggedBuild(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/" + codec.AssetName
	if err := os.WriteFile(f, []byte("not a wasm module"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(codec.AssetEnv, f)

	path, err := codec.AssetPath()
	if taggedBuild {
		if err != nil {
			t.Fatalf("AssetPath() error = %v, want nil", err)
		}
		if path != f {
			t.Fatalf("AssetPath() = %q, want %q", path, f)
		}
		return
	}
	if err == nil {
		t.Fatalf("AssetPath() = %q, nil; the untagged build has no codec", path)
	}
	if !strings.Contains(err.Error(), f) {
		t.Fatalf("AssetPath() error = %v, want it to quote the configured path", err)
	}
}

func TestAssetPathRejectsRelativeOverride(t *testing.T) {
	if !taggedBuild {
		t.Skip("the untagged build never accepts an override")
	}
	t.Setenv(codec.AssetEnv, "dgwcppbridge.wasm")
	if _, err := codec.AssetPath(); err == nil {
		t.Fatal("AssetPath() accepted a relative override")
	}
}

func TestRequiredSymbolsTable(t *testing.T) {
	if len(codec.RequiredSymbols) == 0 {
		t.Fatal("RequiredSymbols is empty")
	}
	seen := make(map[string]bool, len(codec.RequiredSymbols))
	for _, s := range codec.RequiredSymbols {
		if s == "" {
			t.Fatal("RequiredSymbols has an empty entry")
		}
		if seen[s] {
			t.Fatalf("RequiredSymbols repeats %q", s)
		}
		seen[s] = true
	}
	for _, s := range []string{
		codec.SymbolMalloc,
		codec.SymbolFree,
		codec.SymbolEncodeData,
		codec.SymbolEncodeEstab,
		codec.SymbolDecode,
	} {
		if !seen[s] {
			t.Fatalf("RequiredSymbols omits %q", s)
		}
	}
}

// The default build has no codec, so the dgw seam must reject a nil one rather
// than panic; this ties the package to the interface it is meant to satisfy.
func TestUnavailableCodecIsNotAUsableSeam(t *testing.T) {
	var c dgw.Codec
	if _, err := codec.New(); err == nil {
		c = nil
	}
	if c != nil {
		t.Fatal("New() succeeded without the wasm asset")
	}
	_, err := dgw.New(dgw.Options{})
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("dgw.New() error = %v, want backend.ErrUnavailable", err)
	}
}
