// Tests for the DGW golden-vector loader and the experimental backend.
//
// These tests assert only that committed fixtures load and round-trip through a
// Codec, and that the harness's own bookkeeping (uniqueness, grouping, typed
// errors) holds. NO test in this file asserts interoperability with the live
// meta.ai gateway: this repository has no DGW codec, so interoperability cannot
// be exercised here. The payloads the tests read are recorded-session fixtures;
// no code in this package authors frame bytes.
package dgw_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/dgw"
)

// The fixtures record the JSON the real codec produced for each payload. To
// check the loader against those recorded values byte-for-byte the tests need a
// codec, and this repository has none: echoCodec decodes a payload to the JSON
// recorded for it and encodes that JSON straight back to the recorded bytes. It
// proves the loader compares exact bytes and exact JSON; it proves nothing
// about the wire format.
type echoCodec struct {
	byB64   map[string]json.RawMessage
	byValue map[string][]byte
}

func newEchoCodec(t *testing.T, vectors []dgw.GoldenVector) *echoCodec {
	t.Helper()
	c := &echoCodec{
		byB64:   make(map[string]json.RawMessage, len(vectors)),
		byValue: make(map[string][]byte, len(vectors)),
	}
	for _, v := range vectors {
		if v.Fail != "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			t.Fatalf("fixture %s: raw_b64: %v", v.ID, err)
		}
		var compact bytes.Buffer
		if err := json.Compact(&compact, v.DecodedJSON); err != nil {
			t.Fatalf("fixture %s: decoded_json: %v", v.ID, err)
		}
		c.byB64[v.RawB64] = compact.Bytes()
		c.byValue[string(compact.Bytes())] = raw
	}
	return c
}

func (c *echoCodec) Decode(raw []byte) (dgw.Frame, error) {
	rec, ok := c.byB64[base64.StdEncoding.EncodeToString(raw)]
	if !ok {
		return dgw.Frame{}, errors.New("echo codec: payload not in fixture")
	}
	return dgw.Frame{Type: dgw.FrameStreamGroupData, Payload: rec}, nil
}

func (c *echoCodec) Encode(f dgw.Frame) ([]byte, error) {
	rec, ok := c.byValue[string(f.Payload)]
	if !ok {
		return nil, errors.New("echo codec: payload not in fixture")
	}
	return append([]byte(nil), rec...), nil
}

func (c *echoCodec) EncodePing() ([]byte, error) { return nil, errors.New("echo codec: no ping") }

func (c *echoCodec) EncodePong(dgw.Frame) ([]byte, error) {
	return nil, errors.New("echo codec: no pong")
}

// loadVectors loads the fixture and fails the test if it does not parse.
func loadVectors(t *testing.T, path string) []dgw.GoldenVector {
	t.Helper()
	set, err := dgw.LoadGolden(path)
	if err != nil {
		t.Fatalf("LoadGolden(%s): %v", path, err)
	}
	if len(set.Frames) == 0 {
		t.Fatal("fixture has no vectors")
	}
	return set.Frames
}

func TestGoldenDecode(t *testing.T) {
	vectors := loadVectors(t, "testdata/golden.example.json")
	codec := newEchoCodec(t, vectors)

	for _, v := range vectors {
		if v.Fail != "" {
			continue
		}
		if !v.Direction.Valid() {
			t.Fatalf("fixture %s: direction %q", v.ID, v.Direction)
		}
		if _, ok := dgw.ParseFrameTypeName(v.Type); !ok {
			t.Fatalf("fixture %s: type %q is not a known frame type", v.ID, v.Type)
		}
		raw, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			t.Fatalf("fixture %s: raw_b64: %v", v.ID, err)
		}
		frame, err := codec.Decode(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", v.ID, err)
		}
		if !jsonEqual(frame.Payload, v.DecodedJSON) {
			t.Errorf("decode %s: payload = %s, want %s", v.ID, frame.Payload, v.DecodedJSON)
		}
	}
}

func TestGoldenEncode(t *testing.T) {
	vectors := loadVectors(t, "testdata/golden.example.json")
	codec := newEchoCodec(t, vectors)

	for _, v := range vectors {
		if v.Fail != "" {
			continue
		}
		want, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			t.Fatalf("fixture %s: raw_b64: %v", v.ID, err)
		}
		ft, _ := dgw.ParseFrameTypeName(v.Type)
		var compact bytes.Buffer
		if err := json.Compact(&compact, v.DecodedJSON); err != nil {
			t.Fatalf("fixture %s: decoded_json: %v", v.ID, err)
		}
		got, err := codec.Encode(dgw.Frame{Type: ft, Payload: compact.Bytes()})
		if err != nil {
			t.Fatalf("encode %s: %v", v.ID, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("encode %s: bytes = %x, want %x", v.ID, got, want)
		}
	}
}

func TestGoldenRoundTrip(t *testing.T) {
	vectors := loadVectors(t, "testdata/golden.example.json")
	codec := newEchoCodec(t, vectors)

	for _, v := range vectors {
		if v.Fail != "" {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			t.Fatalf("fixture %s: raw_b64: %v", v.ID, err)
		}
		frame, err := codec.Decode(raw)
		if err != nil {
			t.Fatalf("decode %s: %v", v.ID, err)
		}
		back, err := codec.Encode(frame)
		if err != nil {
			t.Fatalf("encode %s: %v", v.ID, err)
		}
		if !bytes.Equal(back, raw) {
			t.Errorf("%s: round trip = %x, want %x", v.ID, back, raw)
		}
	}
}

func TestGoldenGroupingEndsOnTerminalFrame(t *testing.T) {
	set, err := dgw.LoadGolden("testdata/golden.example.json")
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}

	var inbound, stream int
	for _, v := range set.Frames {
		if v.Direction == dgw.DirectionInbound {
			inbound++
		}
	}
	if inbound == 0 {
		t.Fatal("no inbound frames: grouping is exercised only for inbound traffic")
	}

	conversation := "conv-0001-stream"
	got := dgw.GroupFrames(set.Frames, conversation)
	for _, f := range got {
		if f.ConversationID != conversation {
			t.Errorf("grouped frame %v has conversation %q, want %q", f.Frame.Type, f.ConversationID, conversation)
		}
		if f.Direction != dgw.DirectionInbound {
			t.Errorf("grouped frame %v is %s, want inbound", f.Frame.Type, f.Direction)
		}
		if f.Raw == nil && f.Frame.Type != dgw.FrameEmpty {
			t.Errorf("grouped frame %v lost its raw bytes", f.Frame.Type)
		}
		if strings.HasPrefix(f.Frame.Type.String(), "StreamGroup_") {
			stream++
		}
	}
	if stream == 0 {
		t.Fatal("no StreamGroup frames in the group")
	}
	var last dgw.FrameType
	for _, f := range got {
		if f.Frame.Type.Known() {
			last = f.Frame.Type
		}
	}
	if !last.Terminal() {
		t.Fatalf("last known grouped frame = %v, which does not end the group", last)
	}
}

func TestGoldenUnmappedFrameFails(t *testing.T) {
	set, err := dgw.LoadGolden("testdata/golden.example.json")
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}
	vectors := set.Frames

	// The echo codec stands in for the real decoder. Its empty-table fallback
	// is the unmapped path: a payload with no recorded frame type.
	var failing []dgw.GoldenVector
	for _, v := range vectors {
		if v.Fail != "" {
			failing = append(failing, v)
		}
	}
	if len(failing) == 0 {
		t.Fatal("fixture has no failing vector: the unmapped-type path is untested")
	}

	codec := dgw.NewUnmappedCodec(nil)
	for _, v := range failing {
		raw, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			t.Fatalf("fixture %s: raw_b64: %v", v.ID, err)
		}
		_, err = codec.Decode(raw)
		if !errors.Is(err, dgw.ErrUnmappedFrame) {
			t.Fatalf("%s: Decode error = %v, want ErrUnmappedFrame", v.ID, err)
		}
		var fe *dgw.FrameError
		if !errors.As(err, &fe) {
			t.Fatalf("%s: Decode error = %T, want *dgw.FrameError", v.ID, err)
		}
		if !strings.Contains(err.Error(), v.Fail) {
			t.Fatalf("%s: error %q does not name %q", v.ID, err, v.Fail)
		}
	}
}

func TestLoadGoldenRejectsMissing(t *testing.T) {
	_, err := dgw.LoadGolden(filepath.Join(t.TempDir(), "absent.json"))
	if !errors.Is(err, dgw.ErrGoldenMissing) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenMissing", err)
	}
	if !errors.Is(err, backend.ErrUnavailable) {
		t.Fatalf("LoadGolden error %v must also match backend.ErrUnavailable", err)
	}
}

func TestLoadGoldenRejectsBadBase64(t *testing.T) {
	path := writeGolden(t, `{"version":1,"source":"test","frames":[
		{"id":"a","direction":"inbound","type":"Ping","description":"d","raw_b64":"@@@"}]}`)
	_, err := dgw.LoadGolden(path)
	if !errors.Is(err, dgw.ErrGoldenInvalid) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenInvalid", err)
	}
	if !strings.Contains(err.Error(), `"a"`) {
		t.Fatalf("error %q does not name the offending frame", err)
	}
}

func TestLoadGoldenRejectsDuplicateID(t *testing.T) {
	path := writeGolden(t, `{"version":1,"source":"test","frames":[
		{"id":"a","direction":"inbound","type":"Ping","description":"d","raw_b64":"CQ=="},
		{"id":"a","direction":"inbound","type":"Ping","description":"d","raw_b64":"CQ=="}]}`)
	_, err := dgw.LoadGolden(path)
	if !errors.Is(err, dgw.ErrGoldenInvalid) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenInvalid", err)
	}
}

func TestLoadGoldenRejectsUnknownDirection(t *testing.T) {
	path := writeGolden(t, `{"version":1,"source":"test","frames":[
		{"id":"a","direction":"sideways","type":"Ping","description":"d","raw_b64":"CQ=="}]}`)
	_, err := dgw.LoadGolden(path)
	if !errors.Is(err, dgw.ErrGoldenInvalid) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenInvalid", err)
	}
}

func TestLoadGoldenRejectsUndecodableJSON(t *testing.T) {
	path := writeGolden(t, `{"version":1,"source":"test","frames":[
		{"id":"a","direction":"inbound","type":"Ping","description":"d","raw_b64":"CQ==","decoded_json":{"x":}}]}`)
	_, err := dgw.LoadGolden(path)
	if !errors.Is(err, dgw.ErrGoldenInvalid) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenInvalid", err)
	}
}

func TestLoadGoldenRejectsEmptyType(t *testing.T) {
	path := writeGolden(t, `{"version":1,"source":"test","frames":[
		{"id":"a","direction":"inbound","type":"","description":"d","raw_b64":"CQ=="}]}`)
	_, err := dgw.LoadGolden(path)
	if !errors.Is(err, dgw.ErrGoldenInvalid) {
		t.Fatalf("LoadGolden error = %v, want ErrGoldenInvalid", err)
	}
}

// jsonEqual reports whether a and b hold the same JSON value, ignoring
// formatting.
func jsonEqual(a, b []byte) bool {
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

func writeGolden(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "golden.json")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestGoldenSetSource(t *testing.T) {
	set, err := dgw.LoadGolden("testdata/golden.example.json")
	if err != nil {
		t.Fatalf("LoadGolden: %v", err)
	}
	if set.Source == "" {
		t.Fatal("golden set has no source: a vector must name the session it came from")
	}
}
