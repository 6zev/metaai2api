// Package ectostream decodes the assistant's reply stream from the meta.ai
// gateway into plain deltas.
//
// The pieces it combines:
//
//	websocket message  -> dgw.DecodeWireFrame        (verified against recordings)
//	                     JSON envelope {req-id, payload}, base64
//	                  -> clippy.ecto.Response         (ectoproto, from the descriptor)
//	                  -> Delta                        (this package)
//
// What is verified: the frame layer against four recorded frames, the protobuf
// schema against the descriptor shipped in the client bundle (the generated code
// is regenerated from that descriptor by a test), and one inbound message the
// server actually sent (testdata/recorded-inbound-ack-frame.bin, an async ack).
//
// What is NOT verified: a live text/reasoning/deltas reply. Only the opening
// acknowledgement was captured inbound, so answer text is exercised against
// payloads built from the same generated types. Record a frame carrying
// ModelResponse.raw_chunk to close that gap.
package ectostream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/me/metaai2api/internal/meta/dgw"
	"github.com/me/metaai2api/internal/meta/dgw/ectoproto"
	"google.golang.org/protobuf/proto"
)

// ErrNotResponse reports a frame whose body is not a Response.
var ErrNotResponse = errors.New("ectostream: frame does not carry a response")

// Kind classifies a delta.
type Kind string

// Delta kinds.
const (
	// KindText is assistant answer text to append.
	KindText Kind = "text"
	// KindReasoning is reasoning-trace text, shown separately from the answer.
	KindReasoning Kind = "reasoning"
	// KindStatic is a fixed reply that replaces the model's answer.
	KindStatic Kind = "static"
	// KindProgress is a status line describing what the agent is doing.
	KindProgress Kind = "progress"
	// KindAck acknowledges the prompt was received.
	KindAck Kind = "ack"
	// KindTitle carries the conversation title chosen after the reply.
	KindTitle Kind = "title"
	// KindError is a user-facing failure.
	KindError Kind = "error"
	// KindDone marks the end of the answer.
	KindDone Kind = "done"
)

// Delta is one decoded piece of a reply.
type Delta struct {
	Kind Kind
	// Text is the content for KindText, KindReasoning, KindStatic, KindTitle,
	// KindProgress and KindError. Empty for KindDone and KindAck.
	Text string
	// ResponseID identifies the reply this delta belongs to.
	ResponseID string
	// ErrorCode is set for KindError.
	ErrorCode ectoproto.ErrorCode
	// Final is set when the stream signalled completion.
	Final bool
}

// envelope is the JSON wrapper the gateway puts around a protobuf body. The
// client states this with x-dgw-app-client-payload-type: PROTO_INSIDE_JSON.
type envelope struct {
	ReqID   string `json:"req-id"`
	Payload string `json:"payload"`
}

// DecodeEnvelope unwraps the base64 protobuf inside a Data frame body.
//
// A body that is already protobuf (the server may not wrap inbound frames the
// same way the client wraps outbound ones) is accepted as-is, which is why this
// returns the raw bytes rather than a decoded message.
func DecodeEnvelope(body []byte) ([]byte, error) {
	var env envelope
	if err := json.Unmarshal(body, &env); err != nil || env.Payload == "" {
		// Not the JSON envelope: treat the body as protobuf.
		return body, nil
	}
	raw, err := base64.StdEncoding.DecodeString(env.Payload)
	if err != nil {
		return nil, fmt.Errorf("ectostream: envelope payload is not base64: %w", err)
	}
	return raw, nil
}

// DecodeResponse parses one websocket message into zero or more deltas.
//
// It returns no deltas and no error for frames that carry no reply (acks,
// pings, end-of-stream markers), so a caller can loop over messages without
// special-casing control frames.
func DecodeResponse(msg []byte) ([]Delta, error) {
	frame, err := dgw.DecodeWireFrame(msg)
	if err != nil {
		return nil, err
	}
	if len(frame.StreamData) == 0 {
		return nil, nil
	}

	raw, err := DecodeEnvelope(frame.StreamData)
	if err != nil {
		return nil, err
	}

	var resp ectoproto.Response
	if err := proto.Unmarshal(raw, &resp); err != nil {
		// A body that is not a Response is not necessarily an error: control
		// frames carry JSON. Say so rather than failing the stream.
		return nil, fmt.Errorf("%w: %v", ErrNotResponse, err)
	}
	if resp.GetPiece() == nil && resp.GetResponseId() == "" {
		return nil, nil
	}

	return deltasFor(&resp), nil
}

// deltasFor converts one Response into deltas.
func deltasFor(resp *ectoproto.Response) []Delta {
	id := resp.GetResponseId()
	var out []Delta

	if piece := resp.GetPiece(); piece != nil {
		switch v := piece.GetPiece().(type) {
		case *ectoproto.ResponsePiece_ModelResponse:
			// raw_chunk is the incremental text. pp_response_so_far is the
			// whole answer so far, which a caller must not append blindly.
			if t := v.ModelResponse.GetRawChunk(); t != "" {
				out = append(out, Delta{Kind: KindText, Text: t, ResponseID: id})
			}
		case *ectoproto.ResponsePiece_ReasoningResponse:
			if t := v.ReasoningResponse.GetDelta(); t != "" {
				out = append(out, Delta{Kind: KindReasoning, Text: t, ResponseID: id})
			}
		case *ectoproto.ResponsePiece_StaticResponse:
			if t := v.StaticResponse.GetText(); t != "" {
				out = append(out, Delta{Kind: KindStatic, Text: t, ResponseID: id})
			}
		case *ectoproto.ResponsePiece_ProgressIndicator:
			if s := v.ProgressIndicator.GetBigBrainStatus(); s != nil {
				text := s.GetTitle()
				if text == "" {
					text = s.GetDetail()
				}
				if text != "" {
					out = append(out, Delta{Kind: KindProgress, Text: text, ResponseID: id})
				}
			}
		case *ectoproto.ResponsePiece_UserMessageAck:
			out = append(out, Delta{Kind: KindAck, ResponseID: id})
		case *ectoproto.ResponsePiece_AsyncAck:
			out = append(out, Delta{Kind: KindAck, ResponseID: id})
		case *ectoproto.ResponsePiece_Error:
			out = append(out, Delta{
				Kind:       KindError,
				Text:       v.Error.GetMessage(),
				ErrorCode:  v.Error.GetErrorCode(),
				ResponseID: id,
			})
		}
	}

	// A title chosen after the answer completes.
	if am := resp.GetAsyncMetadata(); am != nil && am.GetTitle() != "" {
		out = append(out, Delta{Kind: KindTitle, Text: am.GetTitle(), ResponseID: id})
	}

	if resp.GetIsFinalChunk() || resp.GetIsFinalModelChunk() || resp.GetIsRollback() {
		out = append(out, Delta{Kind: KindDone, ResponseID: id, Final: true})
	}
	return out
}
