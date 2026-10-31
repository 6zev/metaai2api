package dgw

import (
	"errors"
	"fmt"
)

// This file implements the framing layer from recorded bytes.
//
// # Evidence
//
// Two frames recorded off a live meta.ai session (testdata/recorded-*.bin)
// establish the layout:
//
//	StreamGroup_EstabStream:  type(1) streamId(2 LE) len(3 LE)            body
//	StreamGroup_Data:         type(1) streamId(2 LE) len(3 LE) ack(2 LE) body
//
// len is the total byte count AFTER the 6-byte header, so it covers the body
// and, for a Data frame, the 2-byte ack that precedes it. Both samples agree:
// EstabStream declares 127 with a 127-byte body, Data declares 1432 with a
// 2-byte ack plus a 1430-byte body. frame_decoder_test.go asserts both
// byte-for-byte.
//
// # What is still unverified
//
// The streamId width (2 bytes) and the ack width (2 bytes) are inferred from
// samples where streamId happened to be 0 and ack happened to be small. A
// session that multiplexes several conversation streams, or one that issues an
// ack above 0xffff, is not covered by the evidence. Re-record before relying on
// those cases.

// ErrShortFrame reports a frame too short to contain its own header.
var ErrShortFrame = errors.New("dgw: frame shorter than header")

// ErrBadLength reports a length field that does not match the frame size.
var ErrBadLength = errors.New("dgw: frame length does not match payload")

// wireHeaderLen is type(1) + streamId(2) + len(3).
const wireHeaderLen = 6

// wireDataHeaderLen is wireHeaderLen plus the 2-byte ack.
const wireDataHeaderLen = 8

// DecodeWireFrame decodes one websocket message using the layout observed in
// recorded sessions.
//
// It is strict about the length field: a frame whose declared length disagrees
// with the bytes present is an error rather than a best-effort decode, because
// accepting it feeds misaligned bytes into the protobuf layer, where the
// resulting failure is far harder to diagnose.
func DecodeWireFrame(msg []byte) (Frame, error) {
	if len(msg) < wireHeaderLen {
		return Frame{}, fmt.Errorf("%w: have %d bytes, need at least %d",
			ErrShortFrame, len(msg), wireHeaderLen)
	}

	f := Frame{
		Direction: DirectionInbound,
		Type:      FrameType(msg[0]),
		StreamID:  StreamID(uint16(msg[1]) | uint16(msg[2])<<8),
		Raw:       append([]byte(nil), msg...),
	}

	declared := int(msg[3]) | int(msg[4])<<8 | int(msg[5])<<16
	rest := len(msg) - wireHeaderLen

	// len is the count of bytes after the 6-byte header. A Data frame spends
	// the first two of those on an ack, so its body starts at 8.
	if declared != rest {
		return Frame{}, fmt.Errorf("%w: declared %d, frame %d, bytes after header %d",
			ErrBadLength, declared, len(msg), rest)
	}

	switch {
	case f.Type == FrameStreamGroupData:
		if rest < 2 {
			return Frame{}, fmt.Errorf("%w: data frame has %d bytes after header, ack needs 2",
				ErrShortFrame, rest)
		}
		f.RequiresAck = true
		f.AckID = uint32(uint16(msg[6]) | uint16(msg[7])<<8)
		f.StreamData = msg[wireDataHeaderLen:]
	default:
		f.StreamData = msg[wireHeaderLen:]
	}

	// Grouped frames are the ones that came out of a stream-group container;
	// only those carry the header this decoder understands.
	f.Grouped = true
	f.Payload = f.StreamData
	return f, nil
}
