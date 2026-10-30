// Codec helpers and frame-handling types for the experimental DGW harness.
//
// Nothing here parses or produces real DGW bytes: they exist so the harness's
// bookkeeping (classification, heartbeat, typed errors) can be exercised
// without the WASM codec. See the package comment in frames.go.

package dgw

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/me/metaai2api/internal/backend"
)

// errUnavailable is the package's identity for "cannot run". It matches both
// itself and backend.ErrUnavailable so either package's errors.Is works.
var errUnavailable = &unavailableSentinel{}

// unavailableSentinel bridges this package's "cannot run" with
// backend.ErrUnavailable.
type unavailableSentinel struct{}

func (unavailableSentinel) Error() string { return "dgw: unavailable" }

func (unavailableSentinel) Is(target error) bool {
	return target == backend.ErrUnavailable || target == error(unavailableSentinel{})
}

// HeartbeatFrame is the empty frame the heartbeat loop sends when it has no
// codec. The gateway is expected to echo the empty frame back as a liveness
// probe (see 2umt4-28uxtjh.js); the exact reply is unverified.
func HeartbeatFrame() Frame {
	return Frame{Direction: DirectionOutbound, Type: FrameEmpty}
}

// UnmappedCodec satisfies Codec by decoding to the frame type the site's
// classifier would reject. It exists so the harness's typed-error paths can be
// tested without a real codec; it never parses bytes.
type UnmappedCodec struct{}

// NewUnmappedCodec returns an UnmappedCodec. The argument is ignored and kept
// only so callers such as fixtures can pass a golden set without a type switch.
func NewUnmappedCodec(any) *UnmappedCodec { return &UnmappedCodec{} }

// Decode always fails with ErrUnmappedFrame, naming the payload.
func (*UnmappedCodec) Decode(raw []byte) (Frame, error) {
	if n, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
		ft := FrameType(n)
		return Frame{}, &FrameError{Type: ft, Err: ErrUnmappedFrame, Detail: fmt.Sprintf("unmapped frame type %d", n)}
	}
	return Frame{}, &FrameError{Err: ErrUnmappedFrame, Detail: fmt.Sprintf("payload %q is not a frame type", raw)}
}

// Encode always fails with ErrUnmappedFrame.
func (*UnmappedCodec) Encode(f Frame) ([]byte, error) {
	return nil, &FrameError{Type: f.Type, Err: ErrUnmappedFrame}
}

// EncodePing and EncodePong always fail: no bytes can be produced without the
// real codec.
func (*UnmappedCodec) EncodePing() ([]byte, error) { return nil, ErrCodecRequired }
func (*UnmappedCodec) EncodePong(Frame) ([]byte, error) {
	return nil, ErrCodecRequired
}

// NopCodec is a Codec that echoes bytes without parsing them. It is a
// placeholder for tests that need a non-nil codec, and is useless against a
// real gateway.
type NopCodec struct{}

// Decode wraps the payload in an Empty frame without interpreting it.
func (*NopCodec) Decode(raw []byte) (Frame, error) {
	return Frame{Direction: DirectionInbound, Type: FrameEmpty, Raw: raw}, nil
}

// Encode returns the frame's raw bytes unchanged.
func (*NopCodec) Encode(f Frame) ([]byte, error) { return f.Raw, nil }

// EncodePing returns the harness Ping payload.
func (*NopCodec) EncodePing() ([]byte, error) { return TextFrame(FramePing), nil }

// EncodePong returns the harness Pong payload.
func (*NopCodec) EncodePong(Frame) ([]byte, error) { return TextFrame(FramePong), nil }

// TextFrame builds a frame whose type travels as the ASCII decimal number in
// the payload.
//
// WARNING: this is NOT the DGW wire format. The real frame type lives inside a
// protobuf message produced by dgwcppbridge.wasm, which this repository does
// not have. TextFrame exists only so the harness can exercise its bookkeeping
// (classification, heartbeat, typed errors) against an in-process server. Any
// frame it produces is meaningless to the live gateway.
func TextFrame(t FrameType) []byte {
	return []byte(strconv.Itoa(int(t)))
}

// IsPingFrame reports whether raw is the harness's Ping encoding, as produced
// by TextFrame(FramePing). It says nothing about real payloads.
func IsPingFrame(raw []byte) bool {
	return string(raw) == string(TextFrame(FramePing))
}

// FrameError reports one frame that could not be handled. Type is the frame's
// type when it was known (zero otherwise) so a log line can name it.
type FrameError struct {
	Type   FrameType
	Err    error
	Detail string
}

func (e *FrameError) Error() string {
	if e.Detail == "" {
		return fmt.Sprintf("dgw: %v", e.Err)
	}
	return fmt.Sprintf("dgw: %v (%s)", e.Err, e.Detail)
}

// Unwrap exposes the sentinel so errors.Is works.
func (e *FrameError) Unwrap() error { return e.Err }
