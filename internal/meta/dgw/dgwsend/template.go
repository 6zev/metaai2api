// Package dgwsend builds and sends a DGW prompt frame.
//
// # Status
//
// The frame envelope is verified: the recorded outbound prompt frames decode with
// it, and the round trip (build here, decode with dgw.DecodeWireFrame) is
// asserted in tests.
//
// The protobuf body is rebuilt by copying a recorded prompt and replacing the
// fields that must change per request (conversation id, request id, prompt text).
// It is NOT generated from ecto_request_client.proto field by field, because the
// descriptor is available but the client also sends a large block of per-session
// fields (flags, feature toggles, capability lists) that are laborious to
// reproduce and whose absence the server has not been shown to tolerate.
//
// So: a recorded template is required at runtime. This package rewrites a
// template; it cannot synthesise one from nothing.
package dgwsend

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// ErrBadTemplate reports a template that does not have the expected shape.
var ErrBadTemplate = errors.New("dgwsend: template does not match the recorded prompt layout")

// PromptTemplate is a recorded prompt protobuf plus the byte spans that differ
// per request. Spans are recorded once, when the template is loaded, so editing
// a request is a copy plus a few overwrites rather than a protobuf walk.
type PromptTemplate struct {
	raw []byte
	// textSpan is the (offset,length) of the prompt text (field 2.2).
	textSpan span
	// conversationSpan is field 2.1.1: the conversation id.
	conversationSpan span
	// requestIDSpan is top-level field 6: the request id.
	requestIDSpan span
}

type span struct {
	off int
	len int
}

// ParseTemplate indexes a recorded prompt protobuf.
//
// Spans are computed structurally — field numbers and lengths — never by searching
// for the old value. A conversation id appears several times in a prompt (as the
// conversation, as the message's parent, and in the storage metadata), so a
// search-and-replace would rewrite the wrong span and quietly corrupt the frame.
//
// It is strict: a template missing any of the four fields is rejected, because
// sending a prompt with a stale conversation id would land the message in the
// wrong thread.
func ParseTemplate(raw []byte) (*PromptTemplate, error) {
	t := &PromptTemplate{raw: append([]byte(nil), raw...)}

	// field 2 -> 1 -> 1  request id
	outer, outerSpan, err := fieldBytesAt(t.raw, 2, 0)
	if err != nil {
		return nil, fmt.Errorf("%w: no prompt wrapper", ErrBadTemplate)
	}
	msg, msgSpan, err := fieldBytesAt(outer, 1, outerSpan.off)
	if err != nil {
		return nil, fmt.Errorf("%w: no message wrapper", ErrBadTemplate)
	}
	reqID, reqSpan, err := fieldBytesAt(msg, 1, msgSpan.off)
	if err != nil {
		return nil, fmt.Errorf("%w: no request id", ErrBadTemplate)
	}
	t.requestIDSpan = reqSpan

	// field 2 -> 1(conversation entry) -> 2 -> 1 conversation id
	// The conversation entry is a nested message whose field 2 is the
	// conversation record and whose field 1 is the message id.
	convEntry, convEntrySpan, err := fieldBytesAt(msg, 2, msgSpan.off)
	if err != nil {
		return nil, fmt.Errorf("%w: no conversation entry", ErrBadTemplate)
	}
	convID, convSpan, err := fieldBytesAt(convEntry, 1, convEntrySpan.off)
	if err != nil {
		return nil, fmt.Errorf("%w: no conversation id", ErrBadTemplate)
	}
	t.conversationSpan = convSpan

	// field 2 -> 2 prompt text
	text, textSpan, err := fieldBytesAt(outer, 2, outerSpan.off)
	if err != nil {
		return nil, fmt.Errorf("%w: no prompt text", ErrBadTemplate)
	}
	t.textSpan = textSpan

	if len(reqID) == 0 || len(convID) == 0 || len(text) == 0 {
		return nil, fmt.Errorf("%w: a required field is empty", ErrBadTemplate)
	}
	return t, nil
}

// Build returns a prompt protobuf for one request.
//
// prompt is the user text, conversationID the thread it belongs to, and requestID
// the correlation id echoed in the frame envelope. All three must be valid; an
// empty value is refused rather than sent.
//
// Lengths are recomputed, so the prompt may be any length. An earlier version
// spliced bytes in place and worked only when the replacement happened to be the
// same size as the original.
func (t *PromptTemplate) Build(prompt, conversationID, requestID string) ([]byte, error) {
	return buildPrompt(t.raw, prompt, conversationID, requestID)
}

// NewRequestID returns a random UUID-shaped id.
func NewRequestID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant
	s := hex.EncodeToString(b[:])
	return s[0:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:32], nil
}

// ---- minimal protobuf reading ----

// fieldBytesAt returns the value of field num in b, plus the span of that value
// in the ORIGINAL buffer (base + its offset within b).
//
// base lets a caller descend several levels and still get offsets that apply to
// the top-level buffer, which is what the edits need.
func fieldBytesAt(b []byte, num protowire.Number, base int) ([]byte, span, error) {
	off := 0
	for len(b) > 0 {
		n, typ, tagLen := protowire.ConsumeTag(b)
		if tagLen < 0 {
			return nil, span{}, ErrBadTemplate
		}
		if typ == protowire.BytesType {
			v, l := protowire.ConsumeBytes(b[tagLen:])
			if l < 0 {
				return nil, span{}, ErrBadTemplate
			}
			valueOff := off + tagLen + (l - len(v))
			if n == num {
				return v, span{off: base + valueOff, len: len(v)}, nil
			}
			b = b[tagLen+l:]
			off += tagLen + l
			continue
		}
		// Any other wire type is copied past, not treated as an error: the
		// recorded request contains fixed-width fields (the dpr float inside
		// field 19) and skipping them must not fail the lookup.
		var adv int
		switch typ {
		case protowire.VarintType:
			_, adv = protowire.ConsumeVarint(b[tagLen:])
		case protowire.Fixed32Type:
			adv = 4
		case protowire.Fixed64Type:
			adv = 8
		default:
			return nil, span{}, ErrBadTemplate
		}
		if adv <= 0 || tagLen+adv > len(b) {
			return nil, span{}, ErrBadTemplate
		}
		b = b[tagLen+adv:]
		off += tagLen + adv
	}
	return nil, span{}, ErrBadTemplate
}

// ConversationID returns the conversation id the template was recorded with.
func (t *PromptTemplate) ConversationID() string {
	return string(t.raw[t.conversationSpan.off : t.conversationSpan.off+t.conversationSpan.len])
}

// PromptText returns the prompt text the template was recorded with.
func (t *PromptTemplate) PromptText() string {
	return string(t.raw[t.textSpan.off : t.textSpan.off+t.textSpan.len])
}
