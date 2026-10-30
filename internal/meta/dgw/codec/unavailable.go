// New and AssetPath for the build without the wasm_codec tag.
//
// The bridge itself is in wazero.go, which needs the wasm module at compile
// time. This file keeps the same API for every other build: New reports
// backend.ErrUnavailable and never returns a codec.

//go:build !wasm_codec

package codec

import (
	"fmt"
	"os"

	"github.com/me/metaai2api/internal/meta/dgw"
)

// builtIn reports whether this build has the bridge. False here; AssetEnv is
// defined in codec.go for both builds.
const builtIn = false

// New reports that the codec is not built in. A programme can therefore call it
// unconditionally and get a typed error instead of a panic or a nil codec.
func New() (dgw.Codec, error) {
	return nil, errMissingAsset
}

// AssetPath returns the wasm file the build would use, or reports that there is
// none. The untagged build cannot load a module at all, so even a configured
// override is only quoted back in the error, never accepted as a path.
func AssetPath() (string, error) {
	if p := os.Getenv(AssetEnv); p != "" {
		return "", fmt.Errorf("%w (configured %s is not loaded without the wasm_codec build tag)", errMissingAsset, p)
	}
	return "", errMissingAsset
}

// assetPath is AssetPath with a fixed answer for AssetHint, which must work
// without touching the configuration.
func assetPath() string { return compiledAssetPath }
