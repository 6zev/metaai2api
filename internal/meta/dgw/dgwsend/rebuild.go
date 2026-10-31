package dgwsend

import (
	"errors"
	"fmt"

	"google.golang.org/protobuf/encoding/protowire"
)

// This file rebuilds the prompt protobuf properly: it replaces field values AND
// recomputes the length prefix of every enclosing message.
//
// The first implementation spliced bytes in place, which happened to work only
// when the replacement was the same size as the original. Any other length left
// every enclosing length prefix one byte short, the server's protobuf parser read
// past the field, and the frame was silently rejected — which looked exactly like
// an integrity failure and sent this work chasing a hash that was never the
// problem.

// pathStep is one level of the field path to the prompt text.
type pathStep = protowire.Number

// buildPrompt replaces the prompt text, the conversation id and the request id
// inside the recorded request, fixing every length prefix on the way back out.
func buildPrompt(tmpl []byte, prompt, conversationID, requestID string) ([]byte, error) {
	if prompt == "" {
		return nil, errors.New("dgwsend: prompt is empty")
	}
	if conversationID == "" || requestID == "" {
		return nil, errors.New("dgwsend: conversation id and request id are required")
	}

	// Replace innermost first: field 2.2 text, then 2.1.1 conversation id and
	// 2.1.1 request id (they share the same message), then recompute outward.
	out, err := replaceMessage(tmpl, []pathStep{2, 1, 1}, requestID)
	if err != nil {
		return nil, err
	}
	out, err = replaceMessage(out, []pathStep{2, 1, 2, 1}, conversationID)
	if err != nil {
		return nil, err
	}
	return replaceMessage(out, []pathStep{2, 2}, prompt)
}

// replaceMessage walks path and sets the length-delimited field at the end of it
// to value. Every message on the path has its length recomputed.
func replaceMessage(msg []byte, path []pathStep, value string) ([]byte, error) {
	return replaceAt(msg, path, []byte(value))
}

func replaceAt(msg []byte, path []pathStep, value []byte) ([]byte, error) {
	if len(path) == 0 {
		return nil, errors.New("dgwsend: empty path")
	}
	target := path[0]

	var out []byte
	i := 0
	found := false
	for i < len(msg) {
		num, typ, tagLen := protowire.ConsumeTag(msg[i:])
		if tagLen < 0 {
			return nil, fmt.Errorf("%w: bad tag", ErrBadTemplate)
		}
		start := i
		i += tagLen

		if typ == protowire.BytesType {
			body, bodyLen := protowire.ConsumeBytes(msg[i:])
			if bodyLen < 0 {
				return nil, fmt.Errorf("%w: field %d truncated", ErrBadTemplate, num)
			}
			i += bodyLen

			if num == target {
				found = true
				var newBody []byte
				if len(path) == 1 {
					newBody = value
				} else {
					var err error
					if newBody, err = replaceAt(body, path[1:], value); err != nil {
						return nil, err
					}
				}
				// Re-emit tag + recomputed length + body.
				out = append(out, msg[start:start+tagLen]...)
				out = protowire.AppendVarint(out, uint64(len(newBody)))
				out = append(out, newBody...)
				continue
			}
			out = append(out, msg[start:i]...)
			continue
		}

		// Any other wire type: copy through.
		var adv int
		switch typ {
		case protowire.VarintType:
			_, adv = protowire.ConsumeVarint(msg[i:])
		case protowire.Fixed32Type:
			adv = 4
		case protowire.Fixed64Type:
			adv = 8
		default:
			return nil, fmt.Errorf("%w: wire type %d", ErrBadTemplate, typ)
		}
		if adv < 0 || i+adv > len(msg) {
			return nil, fmt.Errorf("%w: truncated value", ErrBadTemplate)
		}
		i += adv
		out = append(out, msg[start:i]...)
	}

	if !found {
		return nil, fmt.Errorf("%w: field %d not present", ErrBadTemplate, target)
	}
	return out, nil
}
