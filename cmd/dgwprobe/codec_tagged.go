//go:build wasm_codec

package main

import (
	"fmt"
	"os"

	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/codec"
)

// availableCodec loads the WASM-backed codec so recorded frames can be
// labelled with their real type. A failure to load is reported loudly: a
// recording labelled "unknown" is still valid, but the operator should know
// why they are not getting types.
func availableCodec() dgw.Codec {
	c, err := codec.New()
	if err != nil {
		fmt.Fprintf(os.Stderr, "dgwprobe: wasm codec unavailable: %v\n", err)
		return nil
	}
	return c
}
