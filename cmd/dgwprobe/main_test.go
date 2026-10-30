package main

import (
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw"
)

// stubCodec stands in for the WASM codec so classify can be tested without the
// absent binary module. It is not a wire implementation: it only proves that
// classify reports whatever the codec returned.
type stubCodec struct {
	decoded dgw.Frame
	decode  error
}

func (s *stubCodec) Decode([]byte) (dgw.Frame, error) { return s.decoded, s.decode }
func (s *stubCodec) Encode(dgw.Frame) ([]byte, error) { return nil, nil }
func (s *stubCodec) EncodePing() ([]byte, error)      { return nil, nil }
func (s *stubCodec) EncodePong(dgw.Frame) ([]byte, error) {
	return nil, nil
}

// TestClassifyWithoutCodecLabelsUnknown pins the honest behaviour: with no
// codec the frame is recorded verbatim and labelled "unknown", never guessed.
func TestClassifyWithoutCodecLabelsUnknown(t *testing.T) {
	raw := []byte{0xde, 0xad}
	frame, label, desc := classify(raw, nil)

	if label != "unknown" {
		t.Fatalf("label = %q, want \"unknown\"", label)
	}
	if !strings.Contains(desc, "no frame codec") {
		t.Fatalf("description should explain the missing codec: %q", desc)
	}
	if frame.Type != 0 {
		t.Fatalf("frame type = %d, want 0 (unset)", frame.Type)
	}
	if string(frame.Raw) != string(raw) {
		t.Fatal("raw bytes must be preserved verbatim")
	}
}

// TestClassifyWithCodecUsesRealType pins that a decoded type is what gets
// recorded, so a golden vector carries a real label once a codec exists.
func TestClassifyWithCodecUsesRealType(t *testing.T) {
	want := dgw.FrameStreamGroupData
	frame, label, _ := classify([]byte{0x01}, &stubCodec{decoded: dgw.Frame{Type: want}})

	if frame.Type != want {
		t.Fatalf("frame type = %v, want %v", frame.Type, want)
	}
	if label != want.String() {
		t.Fatalf("label = %q, want %q", label, want.String())
	}
}

// TestClassifyReportsDecodeFailure pins that a codec error is surfaced in the
// description rather than silently mislabelling the frame.
func TestClassifyReportsDecodeFailure(t *testing.T) {
	_, label, desc := classify([]byte{0x01}, &stubCodec{decode: errStubDecode})

	if label != "unknown" {
		t.Fatalf("label = %q, want \"unknown\"", label)
	}
	if !strings.Contains(desc, errStubDecode.Error()) {
		t.Fatalf("description should quote the decode error: %q", desc)
	}
}

type stubErr string

func (e stubErr) Error() string { return string(e) }

const errStubDecode = stubErr("bad frame")
