// Package main implements cmd/wasmcheck, a one-shot smoke test of the DGW wasm
// codec bridge.
//
// It instantiates dgwcppbridge.wasm, lists the module's exports, encodes a
// known StreamGroup_Data payload, decodes the bytes back and compares. Every
// failure is printed once and the process exits non-zero; nothing panics.
//
// Without the wasm asset (which is not in this repository) or without the
// wasm_codec build tag it prints how to proceed and exits non-zero.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/codec"
)

const (
	// exitOK: the module round-tripped.
	exitOK = 0
	// exitFailure: a codec was built but did not behave.
	exitFailure = 1
	// exitDisabled: this binary cannot bind a codec at all.
	exitDisabled = 2
)

func main() {
	os.Exit(run(context.Background(), os.Stdout, os.Stderr))
}

// run is the whole command, testable without a process exit. It returns exitOK
// on PASS and never panics: every error path is a printable line plus a status.
func run(ctx context.Context, stdout, stderr io.Writer) int {
	if !codec.BuiltIn() {
		fmt.Fprintln(stderr, "wasmcheck: this binary was built without the DGW codec.")
		fmt.Fprintf(stderr, "  build it with -tags wasm_codec; %s\n", codec.AssetHint())
		return exitDisabled
	}

	c, err := codec.New()
	if err != nil {
		fmt.Fprintf(stderr, "wasmcheck: %v\n", err)
		if errors.Is(err, backend.ErrUnavailable) {
			return exitDisabled
		}
		return exitFailure
	}
	full, ok := c.(codec.Codec)
	if !ok {
		fmt.Fprintln(stderr, "wasmcheck: the bound codec does not implement the diagnostics interface")
		return exitFailure
	}
	defer full.Close(ctx)

	asset, err := codec.AssetPath()
	if err != nil {
		fmt.Fprintf(stderr, "wasmcheck: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "wasm: %s\n", asset)

	list, err := full.Exports(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "wasmcheck: %v\n", err)
		return exitFailure
	}
	fmt.Fprintf(stdout, "exports (%d):\n", len(list))
	for _, name := range list {
		fmt.Fprintf(stdout, "  %s\n", name)
	}

	frames, err := roundTrip(ctx, full)
	if err != nil {
		fmt.Fprintf(stderr, "wasmcheck: FAIL: %v\n", err)
		return exitFailure
	}
	for _, f := range frames {
		fmt.Fprintf(stdout, "frame: %s\n", describe(f))
	}
	fmt.Fprintln(stdout, "PASS")
	return exitOK
}

// roundTrip encodes the probe payload as a StreamGroup_Data frame, then decodes
// the module's own bytes back and checks the frame, stream id and payload match.
func roundTrip(ctx context.Context, c codec.Codec) ([]dgw.Frame, error) {
	payload := []byte(`{"probe":"wasmcheck"}`)
	want := dgw.Frame{
		Direction:  dgw.DirectionOutbound,
		Type:       dgw.FrameStreamGroupData,
		StreamID:   dgw.StreamID(1),
		StreamData: payload,
	}
	raw, err := c.EncodeFrame(ctx, want)
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	if len(raw) == 0 {
		return nil, errors.New("encode produced no bytes")
	}

	got, err := c.DecodeAll(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	if len(got) != 1 {
		return got, fmt.Errorf("decoded %d frames from one encoded frame", len(got))
	}
	f := got[0]
	switch {
	case f.Type != want.Type:
		return got, fmt.Errorf("decoded type %v, want %v", f.Type, want.Type)
	case f.StreamID != want.StreamID:
		return got, fmt.Errorf("decoded stream id %d, want %d", f.StreamID, want.StreamID)
	case string(f.StreamData) != string(payload):
		return got, fmt.Errorf("decoded payload %q, want %q", f.StreamData, payload)
	}
	return got, nil
}

// describe is the one-line JSON summary the smoke prints for a decoded frame.
func describe(f dgw.Frame) string {
	b, err := json.Marshal(map[string]any{
		"type":      f.Type.String(),
		"stream_id": uint32(f.StreamID),
		"bytes":     len(f.Raw),
	})
	if err != nil {
		return err.Error()
	}
	return string(b)
}
