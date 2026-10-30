// Golden-vector loading for the experimental DGW harness.
//
// A golden vector is a set of frames recorded from a real session. This file
// reads one and checks its shape; it never synthesises bytes. Because the
// repository has no DGW codec, a recorded payload can only be compared against
// the JSON the codec produced for it at recording time — that comparison is what
// the tests do, through a Codec supplied by the caller.
//
// The wire format of these payloads is unverified: the real codec is a
// WebAssembly module (dgwcppbridge.wasm) that is not shipped here.
package dgw

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Sentinel errors for golden-vector loading and frame handling. Every error
// this package returns matches at least one of them via errors.Is.
var (
	// ErrGoldenMissing reports that no golden vector file was supplied or the
	// named file does not exist. It also matches backend.ErrUnavailable because
	// a backend cannot run without a vector.
	ErrGoldenMissing = fmt.Errorf("%w: golden vector missing", errUnavailable)

	// ErrGoldenInvalid reports a golden file whose contents cannot be trusted:
	// bad JSON, bad base64, a duplicate id, an unknown direction or type.
	ErrGoldenInvalid = errors.New("dgw: golden vector invalid")

	// ErrUnmappedFrame reports a frame whose type is not one of the observed
	// DgwFrameType values.
	ErrUnmappedFrame = errors.New("dgw: unmapped frame type")

	// ErrDeprecatedFrame reports one of the pre-StreamGroup frame types, which
	// the site still names but the gateway does not use.
	ErrDeprecatedFrame = errors.New("dgw: deprecated frame type")

	// ErrStreamTerminated reports a control frame that ends the stream
	// (Drain, Deauth, EndOfData). It is a final condition, not a fault.
	ErrStreamTerminated = errors.New("dgw: stream terminated")

	// ErrCodecRequired reports that an operation needs a Codec but none was
	// supplied.
	ErrCodecRequired = errors.New("dgw: codec required")
)

// GoldenSet is one recorded session's frames.
type GoldenSet struct {
	// Version is the fixture schema version; only 1 is accepted.
	Version int `json:"version"`
	// Source names the session the frames were recorded from.
	Source string `json:"source"`
	// Frames are the recorded frames, in recording order.
	Frames []GoldenVector `json:"frames"`
}

// GoldenVector is one recorded frame.
type GoldenVector struct {
	ID          string          `json:"id"`
	Direction   Direction       `json:"direction"`
	Type        string          `json:"type"`
	Description string          `json:"description"`
	RawB64      string          `json:"raw_b64"`
	DecodedJSON json.RawMessage `json:"decoded_json"`
	// Fail, when non-empty, says decoding this payload must fail and names
	// what the error must mention.
	Fail string `json:"fail"`
}

// LoadGolden reads and validates the golden vector at path.
//
// A missing path or missing file is ErrGoldenMissing (and ErrUnavailable). A
// file that loads but breaks a rule — bad base64, duplicate id, unknown
// direction, empty type, invalid decoded_json — is ErrGoldenInvalid naming the
// offending frame. Nothing is dropped silently.
func LoadGolden(path string) (*GoldenSet, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: no golden path configured", ErrGoldenMissing)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("%w: %s does not exist", ErrGoldenMissing, path)
		}
		return nil, fmt.Errorf("%w: read %s: %v", ErrGoldenMissing, path, err)
	}

	var set GoldenSet
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&set); err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrGoldenInvalid, path, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: %s: trailing content after the golden object", ErrGoldenInvalid, path)
	}
	if set.Version != 1 {
		return nil, fmt.Errorf("%w: %s: unsupported version %d", ErrGoldenInvalid, path, set.Version)
	}
	if strings.TrimSpace(set.Source) == "" {
		return nil, fmt.Errorf("%w: %s: empty source", ErrGoldenInvalid, path)
	}

	seen := make(map[string]bool, len(set.Frames))
	for i, v := range set.Frames {
		if err := validateVector(v); err != nil {
			return nil, fmt.Errorf("%w: %s: frame %d (%q): %v", ErrGoldenInvalid, path, i, v.ID, err)
		}
		if seen[v.ID] {
			return nil, fmt.Errorf("%w: %s: frame %d: duplicate id %q", ErrGoldenInvalid, path, i, v.ID)
		}
		seen[v.ID] = true
	}
	if len(set.Frames) == 0 {
		return nil, fmt.Errorf("%w: %s: no frames", ErrGoldenInvalid, path)
	}
	return &set, nil
}

func validateVector(v GoldenVector) error {
	if strings.TrimSpace(v.ID) == "" {
		return errors.New("empty id")
	}
	if !v.Direction.Valid() {
		return fmt.Errorf("direction %q is not inbound or outbound", v.Direction)
	}
	if strings.TrimSpace(v.Type) == "" {
		return errors.New("empty type")
	}
	if strings.TrimSpace(v.RawB64) == "" {
		return errors.New("empty raw_b64")
	}
	if _, err := base64.StdEncoding.DecodeString(v.RawB64); err != nil {
		return fmt.Errorf("raw_b64: %w", err)
	}
	if len(bytes.TrimSpace(v.DecodedJSON)) > 0 && !json.Valid(v.DecodedJSON) {
		return errors.New("decoded_json is not valid JSON")
	}
	return nil
}

// GroupedFrame pairs a frame with the conversation group it belongs to.
type GroupedFrame struct {
	Frame
	ConversationID string
}

// GroupFrames returns the inbound frames of frames in recording order, each
// tagged with conversationID. Only inbound traffic is grouped: the gateway's
// per-conversation stream is what the backend consumes, and a recorded
// outbound frame is a client request rather than part of a group.
//
// Grouping is bookkeeping, not protocol parsing: a frame whose type is not a
// StreamGroup_* type is still included, because the recorded order is what a
// caller replays.
func GroupFrames(frames []GoldenVector, conversationID string) []GroupedFrame {
	out := make([]GroupedFrame, 0, len(frames))
	for _, v := range frames {
		if v.Direction != DirectionInbound {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(v.RawB64)
		if err != nil {
			// LoadGolden already rejected bad base64; a caller that built
			// vectors by hand gets the loud path instead of a silent drop.
			panic(fmt.Sprintf("dgw: GroupFrames: %q: %v", v.ID, err))
		}
		ft, _ := ParseFrameTypeName(v.Type)
		out = append(out, GroupedFrame{
			Frame:          Frame{Direction: v.Direction, Type: ft, Raw: raw},
			ConversationID: conversationID,
		})
	}
	return out
}

// ParseFrameTypeName resolves a fixture's "type" string to a FrameType. The
// observed names are the DgwFrameType_* suffixes and FrameType.String's
// "FrameType(<n>)" form, which records a payload whose type is unmapped.
func ParseFrameTypeName(name string) (FrameType, bool) {
	for t, n := range frameNames {
		if n == name {
			return t, true
		}
	}
	if rest, ok := strings.CutPrefix(name, "FrameType("); ok {
		if n, err := strconv.Atoi(strings.TrimSuffix(rest, ")")); err == nil {
			return FrameType(n), true
		}
	}
	return 0, false
}
