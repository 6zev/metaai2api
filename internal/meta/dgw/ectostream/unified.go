package ectostream

import (
	"encoding/json"
	"strings"
)

// This file decodes the unified-response stream, which is how meta.ai actually
// delivers an answer.
//
// A reply arrives as JSON inside ModelResponse.raw_unified_response (field 10),
// not as a protobuf DeltasResponse. Two shapes:
//
//	{"seq":0,"type":"full", "response":{"sections":[{"view_model":{
//	    "primitive":{"__typename":"GenAIMarkdownTextUXPrimitive","text":"..."}}}]}}
//	{"seq":1,"type":"patch","operations":[
//	    {"op":"delta","path":"/sections/0/view_model/primitive/text","value":"!"}]}
//
// So the first frame carries the whole answer so far and every later frame is a
// delta appended at a JSON pointer. A decoder that only looks at protobuf fields
// sees nothing at all here, which is exactly what happened before this file
// existed.

// UnifiedFrame is one parsed raw_unified_response message.
type UnifiedFrame struct {
	Seq        int64              `json:"seq"`
	Type       string             `json:"type"`
	Response   *UnifiedResponse   `json:"response,omitempty"`
	Operations []UnifiedOperation `json:"operations,omitempty"`
}

// UnifiedResponse is the full-response payload.
type UnifiedResponse struct {
	ResponseID string           `json:"response_id"`
	Sections   []UnifiedSection `json:"sections"`
}

// UnifiedSection holds one view model in the rendered answer.
type UnifiedSection struct {
	ViewModel UnifiedViewModel `json:"view_model"`
}

// UnifiedViewModel is the rendered primitive for a section.
type UnifiedViewModel struct {
	TypeName  string           `json:"__typename"`
	Primitive UnifiedPrimitive `json:"primitive"`
}

// UnifiedPrimitive carries the text or media of one section.
type UnifiedPrimitive struct {
	TypeName string `json:"__typename"`
	Text     string `json:"text"`
}

// UnifiedOperation is one JSON-pointer edit.
type UnifiedOperation struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	Value json.RawMessage `json:"value"`
}

// textPath is the JSON pointer the answer text accumulates at. Other paths
// (citations, images, tool results) are reported but not treated as answer text.
const textPath = "/sections/0/view_model/primitive/text"

// DecodeUnified turns a raw_unified_response payload into deltas.
//
// The first frame is type "full" and replaces the answer; every later frame is a
// "patch" whose delta operations append to it. Callers that only want text can
// append Delta.Text from KindText deltas in order.
func DecodeUnified(raw string, responseID string) ([]Delta, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var uf UnifiedFrame
	if err := json.Unmarshal([]byte(raw), &uf); err != nil {
		return nil, err
	}

	var out []Delta

	switch uf.Type {
	case "full":
		if uf.Response == nil {
			return nil, nil
		}
		id := uf.Response.ResponseID
		if id == "" {
			id = responseID
		}
		// A "full" message REPLACES the answer rather than extending it: the
		// server sends one at seq 0 and again at the end, the second carrying the
		// complete text. Treating it as an append duplicates the whole answer,
		// so it is marked Replace and the caller must reset its buffer.
		for i, sec := range uf.Response.Sections {
			prim := sec.ViewModel.Primitive
			if prim.Text == "" {
				continue
			}
			out = append(out, Delta{
				Kind:       KindText,
				Text:       prim.Text,
				ResponseID: id,
				Path:       sectionPath(i),
				Op:         "full",
				Replace:    true,
			})
		}
	case "patch":
		for _, op := range uf.Operations {
			d := Delta{
				Kind:       KindDelta,
				Path:       op.Path,
				Op:         op.Op,
				ResponseID: responseID,
			}
			// value is a JSON value; text arrives as a JSON string.
			var s string
			if err := json.Unmarshal(op.Value, &s); err == nil {
				d.Text = s
			} else {
				d.Text = string(op.Value)
			}
			// Only the text pointer counts as answer text.
			if op.Op == "delta" && op.Path == textPath {
				d.Kind = KindText
			}
			out = append(out, d)
		}
	}
	return out, nil
}

func sectionPath(i int) string {
	if i == 0 {
		return textPath
	}
	return textPath + "?section=" + itoa(i)
}
