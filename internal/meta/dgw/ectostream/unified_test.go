package ectostream_test

import (
	"bufio"
	"encoding/hex"
	"os"
	"strings"
	"testing"

	"github.com/me/metaai2api/internal/meta/dgw/ectostream"
)

// realReplyFrames loads the frames recorded from a real prompt sent through
// cmd/dgwsend: the stream establish, the ack, the full response and every patch.
func realReplyFrames(t *testing.T) [][]byte {
	t.Helper()
	f, err := os.Open("../testdata/recorded-reply-frames.txt")
	if err != nil {
		t.Fatalf("open frames: %v", err)
	}
	defer f.Close()

	var out [][]byte
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		b, err := hex.DecodeString(line)
		if err != nil {
			t.Fatalf("bad hex line: %v", err)
		}
		out = append(out, b)
	}
	return out
}

// TestRealReplyProducesAnswerText is the test this decoder was missing: it runs
// every frame a live server actually sent for one prompt and asserts the answer
// text reassembles.
//
// It pins the fact that answer text arrives as JSON in
// ModelResponse.raw_unified_response, not as a protobuf DeltasResponse: a decoder
// that only looks at protobuf fields returns nothing for these frames.
func TestRealReplyProducesAnswerText(t *testing.T) {
	frames := realReplyFrames(t)
	if len(frames) == 0 {
		t.Fatal("no frames recorded")
	}

	// Assemble the answer the way a consumer must: a Replace delta resets the
	// buffer. Appending every text delta blindly produces the answer twice,
	// because the server sends a "full" frame at the start AND at the end.
	var text strings.Builder
	sawDone := false

	for _, raw := range frames {
		deltas, err := ectostream.DecodeResponse(raw)
		if err != nil {
			// Control frames ({code:200}) are not responses; that is expected.
			continue
		}
		for _, d := range deltas {
			switch d.Kind {
			case ectostream.KindText:
				if d.Replace {
					text.Reset()
				}
				text.WriteString(d.Text)
			case ectostream.KindDone:
				sawDone = true
			}
		}
	}

	got := text.String()
	if got == "" {
		t.Fatal("no answer text decoded from the recorded reply")
	}
	// The exact wording is the model's, so assert on stable structure rather
	// than on prose that a different run would change.
	if !strings.Contains(got, "hiiiiii") {
		t.Errorf("answer text does not start from the model's reply: %q", got)
	}
	if !strings.Contains(got, "today") {
		t.Errorf("answer text looks truncated: %q", got)
	}
	if !sawDone {
		t.Error("no completion delta in the recorded reply")
	}
	// A doubled answer is the exact failure a naive append produces.
	if strings.Count(got, "hiiiiii") != 1 {
		t.Errorf("answer is duplicated: %q", got)
	}
	t.Logf("reassembled %d chars:\n%s", len(got), got)
}

// TestUnifiedFullThenPatch pins the two message shapes in isolation, so a change
// to the JSON handling fails here rather than only in the integration test.
func TestUnifiedFullThenPatch(t *testing.T) {
	full := `{"seq":0,"type":"full","response":{"response_id":"r1","sections":[` +
		`{"view_model":{"__typename":"GenAISingleLayoutViewModel","primitive":` +
		`{"__typename":"GenAIMarkdownTextUXPrimitive","text":"Hello"}}}]}}`

	deltas, err := ectostream.DecodeUnified(full, "r1")
	if err != nil {
		t.Fatalf("DecodeUnified(full): %v", err)
	}
	if len(deltas) != 1 || deltas[0].Text != "Hello" || deltas[0].Kind != ectostream.KindText {
		t.Fatalf("full = %+v, want one text delta \"Hello\"", deltas)
	}

	patch := `{"seq":1,"type":"patch","operations":[` +
		`{"op":"delta","path":"/sections/0/view_model/primitive/text","value":" there"},` +
		`{"op":"add","path":"/sections/0/citations","value":[{"url":"x"}]}]}`

	deltas, err = ectostream.DecodeUnified(patch, "r1")
	if err != nil {
		t.Fatalf("DecodeUnified(patch): %v", err)
	}
	if len(deltas) != 2 {
		t.Fatalf("patch = %+v, want 2 deltas", deltas)
	}
	// The text pointer is answer text.
	if deltas[0].Kind != ectostream.KindText || deltas[0].Text != " there" {
		t.Errorf("text op = %+v, want KindText \" there\"", deltas[0])
	}
	// Anything else is an edit the caller must not append to the answer.
	if deltas[1].Kind != ectostream.KindDelta {
		t.Errorf("citation op = %+v, want KindDelta", deltas[1])
	}
}

// TestUnifiedMalformedIsAnError keeps a corrupt payload from being silently
// treated as an empty answer.
func TestUnifiedMalformedIsAnError(t *testing.T) {
	if _, err := ectostream.DecodeUnified("{not json", "r1"); err == nil {
		t.Fatal("malformed unified response should be an error")
	}
}
