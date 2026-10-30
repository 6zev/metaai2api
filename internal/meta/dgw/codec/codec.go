// Package codec bridges the DGW protobuf codec compiled to WebAssembly
// (dgwcppbridge.wasm) into this repository.
//
// The module is a human prerequisite: it is not in the meta.ai capture and it is
// deliberately not committed here (no licence, no provenance). Without it the
// package still builds, and New reports backend.ErrUnavailable. The real bridge
// lives in wazero.go behind the wasm_codec build tag.
//
// Nothing in this package is verified for interoperability with the live
// gateway: the exported symbol names and argument order are read from the site's
// JavaScript loader, the outcome is checked only against the module itself.
package codec

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/dgw"
)

// errMissingAsset reports that dgwcppbridge.wasm is not in the build. It
// matches backend.ErrUnavailable so a caller's errors.Is works.
var errMissingAsset = fmt.Errorf("%w: dgwcppbridge.wasm is absent", backend.ErrUnavailable)

// AssetName is the module file name the site's loader resolves (02h3llx9n0qf1.js
// sets M="dgwcppbridge.wasm").
const AssetName = "dgwcppbridge.wasm"

// AssetEnv names the environment variable that points at dgwcppbridge.wasm. The
// tagged build prefers it over the module in the codec directory (an absolute
// path is required there).
const AssetEnv = "DGW_CODEC_WASM"

// compiledAssetPath is where `make fetch-wasm` puts the module: the codec
// directory, which is what embed_wasm.go embeds and what the tagged build looks
// for when $DGW_CODEC_WASM is unset. The default build only quotes it.
var compiledAssetPath = filepath.Join("internal", "meta", "dgw", "codec", AssetName)

// errBadArgument is what a ReturnCodeError matches when the module refused an
// argument (DgwCodecReturnCode_InvalidArgument), as opposed to a module fault.
var errBadArgument = errors.New("dgw codec: invalid argument")

// Exported symbol names the bridge requires, exactly as the JavaScript loader
// exports them. The table is the whole ABI the bridge uses; the bridge checks
// every name here is exported before it calls anything, and fails with
// MissingSymbolError naming the first one that is not.
var (
	SymbolMalloc        = "__malloc"
	SymbolFree          = "__free"
	SymbolEncodeEmpty   = "__DgwCodecEncodeEmpty"
	SymbolEncodeDeauth  = "__DgwCodecEncodeDeauth"
	SymbolEncodeDrain   = "__DgwCodecEncodeDrain"
	SymbolEncodePing    = "__DgwCodecEncodePing"
	SymbolEncodePong    = "__DgwCodecEncodePong"
	SymbolEncodeAck     = "__DgwCodecEncodeStreamGroup_Ack"
	SymbolEncodeData    = "__DgwCodecEncodeStreamGroup_Data"
	SymbolEncodeEndData = "__DgwCodecEncodeStreamGroup_EndOfData"
	SymbolEncodeEstab   = "__DgwCodecEncodeStreamGroup_EstabStream"
	SymbolDecode        = "__DgwCodecDecode"
)

// RequiredSymbols lists every export the bridge calls, in the order it reports
// them. A module missing one is unusable and is rejected at construction.
var RequiredSymbols = []string{
	SymbolMalloc,
	SymbolFree,
	SymbolEncodeEmpty,
	SymbolEncodeDeauth,
	SymbolEncodeDrain,
	SymbolEncodePing,
	SymbolEncodePong,
	SymbolEncodeAck,
	SymbolEncodeData,
	SymbolEncodeEndData,
	SymbolEncodeEstab,
	SymbolDecode,
}

// Codec is the wasm-backed frame codec. It satisfies dgw.Codec, which is what
// the rest of the repository consumes; the extra methods expose the whole frame
// enum and the decode result, which dgw.Codec's narrow shape does not carry.
type Codec interface {
	dgw.Codec

	// EncodeFrame serialises one frame. StreamGroup_* frames use frame.StreamID
	// and frame.StreamData; Empty, Deauth, Drain, Ping and Pong must not carry
	// a stream id.
	EncodeFrame(ctx context.Context, frame dgw.Frame) ([]byte, error)
	// DecodeAll decodes every frame packed in one WebSocket message, in order.
	DecodeAll(ctx context.Context, raw []byte) ([]dgw.Frame, error)
	// Exports lists the module's exports, sorted, for diagnostics.
	Exports(ctx context.Context) ([]string, error)
	// Close releases the module. It is safe to call more than once.
	Close(ctx context.Context) error
}

// BuiltIn reports whether this build has the bridge compiled in. It says nothing
// about whether the module loads; New answers that.
func BuiltIn() bool {
	return builtIn
}

// AssetHint says where a missing module is expected and how to supply it.
func AssetHint() string {
	return "expected " + assetPath() + " (override with $" + AssetEnv + ")"
}
