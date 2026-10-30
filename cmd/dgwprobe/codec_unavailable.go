//go:build !wasm_codec

package main

import "github.com/me/metaai2api/internal/meta/dgw"

// availableCodec returns nil on a default build: the real codec lives in
// dgwcppbridge.wasm, which is not committed. dgwprobe then records raw bytes
// and labels them "unknown" rather than guessing a frame type.
func availableCodec() dgw.Codec { return nil }
