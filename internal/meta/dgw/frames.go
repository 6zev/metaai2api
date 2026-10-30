// Package dgw holds the experimental meta.ai DGW WebSocket harness.
//
// WARNING: the frame wire format is UNVERIFIED. The real codec is a protobuf
// implementation compiled to WebAssembly (dgwcppbridge.wasm) that is not
// available in this repository, so nothing here encodes or decodes frame bytes.
// This package only carries the frame enum and envelope facts read out of the
// site's JavaScript, and a Codec interface that a future WASM bridge satisfies.
// Bytes handled by this package are recorded from a real session (a golden
// vector) or supplied by an external Codec; they are never authored here.
//
// Nothing in this package is verified for interoperability with the live
// gateway. Treat it as a recording harness, not a working transport.
package dgw

import (
	"fmt"
)

// FrameType is a DgwFrameType value as observed in the site's JavaScript.
type FrameType int32

// Frame types, from 2umt4-28uxtjh.js. The values are what the capture calls
// DgwFrameType_*; the numbering is dense and the named members are the complete
// observed set.
const (
	FrameEmpty                  FrameType = 2
	FrameDrain                  FrameType = 3
	FrameDeauth                 FrameType = 4
	FrameDeprecatedEstabStream  FrameType = 5
	FrameDeprecatedData         FrameType = 6
	FrameSmallAck               FrameType = 7
	FrameDeprecatedEndOfData    FrameType = 8
	FramePing                   FrameType = 9
	FramePong                   FrameType = 10
	FrameStreamGroupAck         FrameType = 12
	FrameStreamGroupData        FrameType = 13
	FrameStreamGroupEndOfData   FrameType = 14
	FrameStreamGroupEstabStream FrameType = 15
)

var frameNames = map[FrameType]string{
	FrameEmpty:                  "Empty",
	FrameDrain:                  "Drain",
	FrameDeauth:                 "Deauth",
	FrameDeprecatedEstabStream:  "DeprecatedEstabStream",
	FrameDeprecatedData:         "DeprecatedData",
	FrameSmallAck:               "SmallAck",
	FrameDeprecatedEndOfData:    "DeprecatedEndOfData",
	FramePing:                   "Ping",
	FramePong:                   "Pong",
	FrameStreamGroupAck:         "StreamGroup_Ack",
	FrameStreamGroupData:        "StreamGroup_Data",
	FrameStreamGroupEndOfData:   "StreamGroup_EndOfData",
	FrameStreamGroupEstabStream: "StreamGroup_EstabStream",
}

// String returns the JavaScript-side name, or "FrameType(<n>)" when unmapped.
func (t FrameType) String() string {
	if name, ok := frameNames[t]; ok {
		return name
	}
	return fmt.Sprintf("FrameType(%d)", int32(t))
}

// Known reports whether t is one of the observed frame types.
func (t FrameType) Known() bool {
	_, ok := frameNames[t]
	return ok
}

// Terminal reports whether a frame of this type ends the stream or the session.
// Drain and Deauth are control frames that end it; the three group end frames
// close a per-conversation stream.
func (t FrameType) Terminal() bool {
	switch t {
	case FrameDrain, FrameDeauth, FrameDeprecatedEndOfData, FrameStreamGroupEndOfData:
		return true
	}
	return false
}

// Frame is one WebSocket message.
type Frame struct {
	Direction Direction
	Type      FrameType
	// Raw is the message payload exactly as it arrived. The first bytes carry
	// the frame type in a format this package does not parse; see the package
	// comment.
	Raw []byte
	// Payload is the JSON a Codec produced for Raw, when the caller decoded it.
	Payload []byte
}

// Direction says which way a frame travelled.
type Direction string

// Frame directions.
const (
	// DirectionInbound marks a frame the client received from the gateway.
	DirectionInbound Direction = "inbound"
	// DirectionOutbound marks a frame the client sent to the gateway.
	DirectionOutbound Direction = "outbound"
)

// Valid reports whether d is a known direction.
func (d Direction) Valid() bool {
	return d == DirectionInbound || d == DirectionOutbound
}

// Codec converts between DGW frames and bytes. It is the seam for the real
// protobuf codec: task 11 supplies an implementation backed by
// dgwcppbridge.wasm through wazero.
//
// Every path that moves a frame through bytes in this package goes through a
// Codec (the heartbeat uses the caller's codec when one is supplied), so no
// other part of the package needs to know the wire format.
type Codec interface {
	// Decode parses one message payload into a frame.
	Decode(raw []byte) (Frame, error)
	// Encode serialises one frame into a message payload.
	Encode(frame Frame) ([]byte, error)
	// EncodePing returns the payload the gateway answers with a Pong, for the
	// ready transport connection.
	EncodePing() ([]byte, error)
	// EncodePong answers a received Ping.
	EncodePong(ping Frame) ([]byte, error)
}

// DataPayload is the JSON body a StreamGroup_Data frame carries in a captured
// session. Field names come from 16n4cam67ld35.js; every field is optional
// because the capture shows a subset per frame.
type DataPayload struct {
	ConversationID  string `json:"conversationId,omitempty"`
	PromptContent   string `json:"promptContent,omitempty"`
	UserID          string `json:"userId,omitempty"`
	AppID           string `json:"appId,omitempty"`
	RequestID       string `json:"requestId,omitempty"`
	UniqueMessageID string `json:"uniqueMessageId,omitempty"`
	TurnID          string `json:"turnId,omitempty"`
	AgentType       string `json:"agentType,omitempty"`
	AgentTypeValue  string `json:"agentTypeValue,omitempty"`
}
