package ectostream_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw/ectoproto"
	"github.com/me/metaai2api/internal/meta/dgw/ectostream"
	"google.golang.org/protobuf/proto"
)

// buildFrame assembles a websocket message the way the recorded frames are
// shaped: type(1) streamId(2 LE) len(3 LE) ack(2 LE) body, with a JSON envelope
// carrying base64 protobuf.
//
// The encoder is deliberately local to this test: the inbound path is what is
// under test, and a shared encoder would let one bug hide the other. The frame
// layout itself is pinned separately against recorded bytes.
func buildFrame(t *testing.T, typ byte, resp *ectoproto.Response) []byte {
	t.Helper()

	raw, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal response: %v", err)
	}
	env, err := json.Marshal(map[string]string{
		"req-id":  "test-request",
		"payload": base64.StdEncoding.EncodeToString(raw),
	})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}

	// len counts everything after the 6-byte header, so it includes the ack.
	n := 2 + len(env)
	out := make([]byte, 0, 8+len(env))
	out = append(out, typ, 0, 0) // type, streamId
	out = append(out, byte(n), byte(n>>8), byte(n>>16))
	out = append(out, 0, 0) // ack
	out = append(out, env...)
	return out
}

func TestDecodeTextDeltas(t *testing.T) {
	resp := &ectoproto.Response{
		ResponseId: "resp-1",
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_ModelResponse{
				ModelResponse: &ectoproto.ModelResponse{RawChunk: "hello"},
			},
		},
	}

	got, err := ectostream.DecodeResponse(buildFrame(t, 13, resp))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d deltas, want 1: %+v", len(got), got)
	}
	if got[0].Kind != ectostream.KindText || got[0].Text != "hello" {
		t.Fatalf("delta = %+v, want text \"hello\"", got[0])
	}
	if got[0].ResponseID != "resp-1" {
		t.Fatalf("response id = %q", got[0].ResponseID)
	}
}

// TestDecodeUsesRawChunkNotPpResponse guards a real trap: pp_response_so_far
// holds the whole answer so far, so a caller appending it would duplicate text.
func TestDecodeUsesRawChunkNotPpResponse(t *testing.T) {
	resp := &ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_ModelResponse{
				ModelResponse: &ectoproto.ModelResponse{
					RawChunk:        " world",
					PpResponseSoFar: "hello world",
				},
			},
		},
	}
	got, err := ectostream.DecodeResponse(buildFrame(t, 13, resp))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Text != " world" {
		t.Fatalf("delta = %+v, want the raw chunk only", got)
	}
}

func TestDecodeReasoningSeparateFromText(t *testing.T) {
	resp := &ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_ReasoningResponse{
				ReasoningResponse: &ectoproto.ReasoningResponse{Delta: "thinking..."},
			},
		},
	}
	got, err := ectostream.DecodeResponse(buildFrame(t, 13, resp))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ectostream.KindReasoning {
		t.Fatalf("delta = %+v, want a reasoning delta", got)
	}
}

func TestDecodeErrorDelta(t *testing.T) {
	resp := &ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_Error{
				Error: &ectoproto.ErrorResponse{
					Message:   "rate limited",
					ErrorCode: ectoproto.ErrorCode_ERROR_CODE_GLOBAL_RATE_LIMITED,
				},
			},
		},
	}
	got, err := ectostream.DecodeResponse(buildFrame(t, 13, resp))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ectostream.KindError {
		t.Fatalf("delta = %+v, want an error delta", got)
	}
	if got[0].Text != "rate limited" {
		t.Fatalf("error text = %q", got[0].Text)
	}
}

func TestDecodeFinalChunkEmitsDone(t *testing.T) {
	resp := &ectoproto.Response{
		ResponseId:   "resp-1",
		IsFinalChunk: true,
	}
	got, err := ectostream.DecodeResponse(buildFrame(t, 13, resp))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ectostream.KindDone || !got[0].Final {
		t.Fatalf("delta = %+v, want a final done delta", got)
	}
}

// TestDecodeIgnoresControlFrames verifies a caller can loop over every message
// without special-casing frames that carry no reply.
func TestDecodeIgnoresControlFrames(t *testing.T) {
	// Ping: type 9, no body at all.
	ping := []byte{9, 0, 0, 0, 0, 0}
	got, err := ectostream.DecodeResponse(ping)
	if err != nil {
		t.Fatalf("ping should not error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("ping produced %d deltas", len(got))
	}

	// EndOfData: type 14, a JSON body that is not a Response.
	eod := []byte{14, 0, 0, 2, 0, 0, '{', '}'}
	got, err = ectostream.DecodeResponse(eod)
	if err == nil {
		// A non-Response body may parse as an empty Response; either way it must
		// not produce a delta.
		if len(got) != 0 {
			t.Fatalf("end-of-data produced %d deltas", len(got))
		}
		return
	}
	if !errors.Is(err, ectostream.ErrNotResponse) {
		t.Fatalf("err = %v, want ErrNotResponse or nil", err)
	}
}

// TestDecodeRawProtobufBody covers a server that does not wrap its frames in the
// JSON envelope: the body is protobuf directly.
func TestDecodeRawProtobufBody(t *testing.T) {
	raw, err := proto.Marshal(&ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_ModelResponse{
				ModelResponse: &ectoproto.ModelResponse{RawChunk: "direct"},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	n := 2 + len(raw)
	msg := append([]byte{13, 0, 0, byte(n), byte(n >> 8), byte(n >> 16), 0, 0}, raw...)

	got, err := ectostream.DecodeResponse(msg)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Text != "direct" {
		t.Fatalf("delta = %+v, want text \"direct\"", got)
	}
}

// TestDecodeRealInboundAckFrame is the only test that decodes a message the
// SERVER actually sent. It closes the gap the package doc mentions: the inbound
// path used to be exercised only against payloads built from our own generated
// types.
//
// The frame is a Response carrying ResponsePiece.async_ack {accepted: true,
// request_id}, sent right after the client's prompt.
func TestDecodeRealInboundAckFrame(t *testing.T) {
	raw, err := os.ReadFile("../testdata/recorded-inbound-ack-frame.bin")
	if err != nil {
		t.Fatalf("read recorded frame: %v", err)
	}

	got, err := ectostream.DecodeResponse(raw)
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d deltas, want 1: %+v", len(got), got)
	}
	if got[0].Kind != ectostream.KindAck {
		t.Fatalf("kind = %q, want %q", got[0].Kind, ectostream.KindAck)
	}
	// The response id must come from the wire, not from a default.
	if got[0].ResponseID != "7996a4a1-1825-45cd-9414-51643bb1cf8a" {
		t.Fatalf("response id = %q", got[0].ResponseID)
	}
	if got[0].Final {
		t.Fatal("an ack is not the end of the stream")
	}
}

// buildFrameFromBody wraps a raw Response body the way the recorded frames are
// shaped: type 13, 2-byte ack, then the body, with len counting everything after
// the 6-byte header.
func buildFrameFromBody(body []byte) []byte {
	n := 2 + len(body)
	out := []byte{13, 0, 0, byte(n), byte(n >> 8), byte(n >> 16), 0, 0}
	return append(out, body...)
}

// TestDecodeDeltasPatch covers the newer streaming shape: a list of JSON-patch
// edits rather than a text chunk. The path that carries answer text is not
// confirmed against a live reply, so the decoder surfaces path/op and leaves the
// interpretation to the caller.
func TestDecodeDeltasPatch(t *testing.T) {
	resp := &ectoproto.Response{
		ResponseId: "r1",
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_Deltas{
				Deltas: &ectoproto.DeltasResponse{
					Deltas: []*ectoproto.Delta{
						{Op: "replace", Path: "/text", Value: "Hi", DeltaIndex: 1},
						{Op: "append", Path: "/text", Value: " there", DeltaIndex: 2},
					},
				},
			},
		},
	}
	raw, err := proto.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := ectostream.DecodeResponse(buildFrameFromBody(raw))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d deltas, want 2: %+v", len(got), got)
	}
	for i, want := range []struct{ op, path, text string }{
		{"replace", "/text", "Hi"},
		{"append", "/text", " there"},
	} {
		if got[i].Kind != ectostream.KindDelta {
			t.Errorf("delta %d kind = %q, want %q", i, got[i].Kind, ectostream.KindDelta)
		}
		if got[i].Op != want.op || got[i].Path != want.path || got[i].Text != want.text {
			t.Errorf("delta %d = {%q %q %q}, want {%q %q %q}",
				i, got[i].Op, got[i].Path, got[i].Text, want.op, want.path, want.text)
		}
		if got[i].DeltaIndex != int64(i+1) {
			t.Errorf("delta %d index = %d, want %d", i, got[i].DeltaIndex, i+1)
		}
	}
}

// TestDecodeInlineResponse covers a piece carrying inline content.
func TestDecodeInlineResponse(t *testing.T) {
	resp := &ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_InlineResponse{
				InlineResponse: &ectoproto.InlineResponse{
					InlineType: "code_block",
					Content:    "fmt.Println()",
				},
			},
		},
	}
	raw, _ := proto.Marshal(resp)
	got, err := ectostream.DecodeResponse(buildFrameFromBody(raw))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ectostream.KindInline {
		t.Fatalf("delta = %+v, want one inline delta", got)
	}
	if got[0].Path != "code_block" || got[0].Text != "fmt.Println()" {
		t.Fatalf("delta = %+v", got[0])
	}
}

// TestDecodeEventResponse covers a named stream event.
func TestDecodeEventResponse(t *testing.T) {
	resp := &ectoproto.Response{
		Piece: &ectoproto.ResponsePiece{
			Piece: &ectoproto.ResponsePiece_EventResponse{
				EventResponse: &ectoproto.EventResponse{
					Event: "search_started",
					Value: "weather",
				},
			},
		},
	}
	raw, _ := proto.Marshal(resp)
	got, err := ectostream.DecodeResponse(buildFrameFromBody(raw))
	if err != nil {
		t.Fatalf("DecodeResponse: %v", err)
	}
	if len(got) != 1 || got[0].Kind != ectostream.KindEvent {
		t.Fatalf("delta = %+v, want one event delta", got)
	}
	if got[0].Path != "search_started" || got[0].Text != "weather" {
		t.Fatalf("delta = %+v", got[0])
	}
}
