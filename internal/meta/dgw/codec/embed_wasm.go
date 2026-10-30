// The embedded copy of dgwcppbridge.wasm, for the tagged build.
//
// The module is not committed: its licence and provenance are unknown (see
// reference/README.md). `make fetch-wasm` downloads it into this directory (or
// DGW_CODEC_WASM points at one), and this file is the only place it is embedded,
// so the default build never needs the file to exist.

//go:build wasm_codec

package codec

import _ "embed"

//go:generate make -C ../../../.. fetch-wasm
//go:embed dgwcppbridge.wasm
var embeddedWasm []byte
