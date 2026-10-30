package codec

import (
	"os"
	"strings"
	"testing"
)

// platformSuffixes are the GOOS/GOARCH values go/build recognises in the
// _<value>.go file-name form.
var platformSuffixes = []string{
	"wasm", "linux", "windows", "darwin", "freebsd", "openbsd", "netbsd",
	"js", "android", "ios", "plan9", "solaris", "aix", "dragonfly", "illumos",
	"amd64", "arm64", "386", "arm", "mips", "mipsle", "mips64", "mips64le",
	"ppc64", "ppc64le", "riscv64", "s390x", "loong64",
}

// TestEmbedFileIsNotAPlatformFile pins the bug that made a wasm-enabled release
// silently carry no codec: Go treats a file named *_<GOOS>.go or *_<GOARCH>.go as
// specific to that platform, so embed_wasm.go was excluded from every linux
// build no matter which build tags were passed, and the asset stopped being
// embedded without any error.
func TestEmbedFileIsNotAPlatformFile(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}

	var embedFiles []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "\n//go:embed ") || strings.HasPrefix(string(body), "//go:embed ") {
			embedFiles = append(embedFiles, e.Name())
		}
	}

	if len(embedFiles) != 1 {
		t.Fatalf("want exactly one file carrying //go:embed, got %v", embedFiles)
	}
	if hasPlatformSuffix(embedFiles[0]) {
		t.Fatalf("%s ends in a GOOS/GOARCH suffix, so Go excludes it from most builds regardless of the wasm_codec tag", embedFiles[0])
	}
}

// hasPlatformSuffix reports whether name ends in _<GOOS>.go or _<GOARCH>.go.
func hasPlatformSuffix(name string) bool {
	base := strings.TrimSuffix(name, ".go")
	for _, s := range platformSuffixes {
		if strings.HasSuffix(base, "_"+s) && len(base) > len(s)+1 {
			return true
		}
	}
	return false
}
