package dgw_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw"
)

// readFrame loads a frame recorded from a live meta.ai session. These are the
// only evidence the wire layout has: there is no specification, and the
// client's own encoder is a WASM module that is not published.
func readFrame(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return b
}

// TestDecodeRecordedDataFrame decodes the real Data frame that carried a "hello"
// prompt. It pins the header layout an encoder must produce.
func TestDecodeRecordedDataFrame(t *testing.T) {
	raw := readFrame(t, "recorded-data-frame.bin")

	f, err := dgw.DecodeWireFrame(raw)
	if err != nil {
		t.Fatalf("DecodeWireFrame: %v", err)
	}

	if f.Type != dgw.FrameStreamGroupData {
		t.Fatalf("type = %v, want StreamGroup_Data", f.Type)
	}
	if f.StreamID != 0 {
		t.Fatalf("streamID = %d, want 0", uint32(f.StreamID))
	}
	if !f.RequiresAck {
		t.Fatal("Data frame should carry an ack field: the declared length includes 2 bytes for it")
	}
	if len(f.StreamData) != 1430 {
		t.Fatalf("stream data = %d bytes, want 1430", len(f.StreamData))
	}
	if !f.Grouped {
		t.Fatal("a StreamGroup_Data frame is grouped")
	}

	// The body is the JSON envelope, not raw protobuf.
	var env struct {
		ReqID   string `json:"req-id"`
		Payload string `json:"payload"`
	}
	if err := json.Unmarshal(f.StreamData, &env); err != nil {
		t.Fatalf("body is not the JSON envelope: %v", err)
	}
	if env.ReqID == "" {
		t.Fatal("envelope has no req-id")
	}
	inner, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		t.Fatalf("envelope payload is not base64: %v", err)
	}
	if len(inner) == 0 {
		t.Fatal("envelope payload decoded to nothing")
	}
}

// TestDecodeRecordedEstabFrame covers the other observed shape: an EstabStream
// frame carries its JSON headers with no ack field.
func TestDecodeRecordedEstabFrame(t *testing.T) {
	raw := readFrame(t, "recorded-estab-frame.bin")

	f, err := dgw.DecodeWireFrame(raw)
	if err != nil {
		t.Fatalf("DecodeWireFrame: %v", err)
	}

	if f.Type != dgw.FrameStreamGroupEstabStream {
		t.Fatalf("type = %v, want StreamGroup_EstabStream", f.Type)
	}
	if f.RequiresAck {
		t.Fatal("EstabStream frame must not carry an ack")
	}
	// 127 is the whole body: the declared length and the body are the same
	// number when there is no ack.
	if len(f.StreamData) != 127 {
		t.Fatalf("stream data = %d bytes, want 127", len(f.StreamData))
	}
	if !strings.HasPrefix(string(f.StreamData), `{"x-dgw-app-x-ecto-conversation-id"`) {
		t.Fatalf("unexpected estab body: %s", f.StreamData)
	}
}

// TestDecodeWireFrameRejectsBadLength is the guard that matters: a length
// mismatch must be an error, because accepting it would feed misaligned bytes
// into the protobuf decoder.
func TestDecodeWireFrameRejectsBadLength(t *testing.T) {
	raw := readFrame(t, "recorded-data-frame.bin")

	bad := append([]byte(nil), raw...)
	bad[3]++
	if _, err := dgw.DecodeWireFrame(bad); !errors.Is(err, dgw.ErrBadLength) {
		t.Fatalf("corrupt length: err = %v, want ErrBadLength", err)
	}
	if _, err := dgw.DecodeWireFrame(raw[:len(raw)-1]); !errors.Is(err, dgw.ErrBadLength) {
		t.Fatalf("truncated: err = %v, want ErrBadLength", err)
	}
	if _, err := dgw.DecodeWireFrame(raw[:3]); !errors.Is(err, dgw.ErrShortFrame) {
		t.Fatalf("short: err = %v, want ErrShortFrame", err)
	}
}

// TestDecodeWireFrameKeepsRaw verifies the recorded bytes survive, because they
// are the evidence and a caller may want to persist them.
func TestDecodeWireFrameKeepsRaw(t *testing.T) {
	raw := readFrame(t, "recorded-estab-frame.bin")
	f, err := dgw.DecodeWireFrame(raw)
	if err != nil {
		t.Fatalf("DecodeWireFrame: %v", err)
	}
	if len(f.Raw) != len(raw) {
		t.Fatalf("raw = %d bytes, want %d", len(f.Raw), len(raw))
	}
	// Mutating the input afterwards must not change the frame's copy.
	raw[0] = 0xff
	if f.Raw[0] == 0xff {
		t.Fatal("Raw aliases the caller's slice")
	}
}
