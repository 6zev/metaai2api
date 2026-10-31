package dgwsend

import (
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protowire"
)

func loadTemplate(t *testing.T) *PromptTemplate {
	t.Helper()
	raw, err := os.ReadFile("../testdata/recorded-prompt.pb")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	tpl, err := ParseTemplate(raw)
	if err != nil {
		t.Fatalf("ParseTemplate: %v", err)
	}
	return tpl
}

// TestBuiltPromptIsStructurallyValid is the regression for the bug that looked
// like an integrity failure: splicing bytes in place left every enclosing length
// prefix short whenever the prompt length changed, so the server's protobuf
// parser read past the field and dropped the frame.
//
// The test walks every length prefix, at any prompt length. It would have caught
// the original defect, which "the same length works" hid completely.
func TestBuiltPromptIsStructurallyValid(t *testing.T) {
	tpl := loadTemplate(t)

	prompts := []string{
		"a",                         // shorter than the template
		"hello",                     // same length as the template
		"hello!",                    // one byte longer
		"hello!!",                   // two longer
		"Xin chào, bạn khỏe không?", // multi-byte UTF-8
		"🎉🎉🎉",                       // 4-byte runes
		"",                          // rejected separately
	}

	for _, p := range prompts {
		p := p
		t.Run(p, func(t *testing.T) {
			out, err := tpl.Build(p, "11111111-2222-3333-4444-555555555555", "req-1")
			if p == "" {
				if err == nil {
					t.Fatal("empty prompt should be rejected")
				}
				return
			}
			if err != nil {
				t.Fatalf("Build: %v", err)
			}
			walkCheck(t, out)
		})
	}
}

// TestBuiltPromptRoundTripsValues asserts the substituted values survive a
// reparse, so a length fix cannot quietly drop them.
func TestBuiltPromptRoundTripsValues(t *testing.T) {
	tpl := loadTemplate(t)
	const (
		prompt = "Xin chào, bạn khỏe không? 🎉"
		conv   = "11111111-2222-3333-4444-555555555555"
		reqID  = "0f0f0f0f-1111-2222-3333-444444444444"
	)

	out, err := tpl.Build(prompt, conv, reqID)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	gotPrompt, _, err := fieldBytesAt(out, 2, 0)
	if err != nil {
		t.Fatalf("field 2: %v", err)
	}
	text, _, err := fieldBytesAt(gotPrompt, 2, 0)
	if err != nil {
		t.Fatalf("field 2.2: %v", err)
	}
	if string(text) != prompt {
		t.Errorf("prompt = %q, want %q", text, prompt)
	}

	msg, _, err := fieldBytesAt(gotPrompt, 1, 0)
	if err != nil {
		t.Fatalf("field 2.1: %v", err)
	}
	entry, _, err := fieldBytesAt(msg, 2, 0)
	if err != nil {
		t.Fatalf("field 2.1.2: %v", err)
	}
	convID, _, err := fieldBytesAt(entry, 1, 0)
	if err != nil {
		t.Fatalf("field 2.1.2.1: %v", err)
	}
	if string(convID) != conv {
		t.Errorf("conversation = %q, want %q", convID, conv)
	}

	gotReq, _, err := fieldBytesAt(msg, 1, 0)
	if err != nil {
		t.Fatalf("field 2.1.1: %v", err)
	}
	if string(gotReq) != reqID {
		t.Errorf("request id = %q, want %q", gotReq, reqID)
	}
}

// TestIntegrityFieldIsPreserved pins that field 19 is copied through untouched.
// It is not a hash of the prompt — three different 5-byte prompts were accepted
// against the same recorded value — so rewriting the prompt must not disturb it.
func TestIntegrityFieldIsPreserved(t *testing.T) {
	tpl := loadTemplate(t)
	out, err := tpl.Build("a completely different prompt", "conv", "req")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	// Field 19 lives at 1.1 (the product context), not at 1.
	integrity, err := integrityField(out)
	if err != nil {
		t.Fatalf("field 1.1.19 missing after rebuild: %v", err)
	}
	origIntegrity, err := integrityField(tpl.raw)
	if err != nil {
		t.Fatalf("field 1.1.19 missing in the template: %v", err)
	}
	if string(integrity) != string(origIntegrity) {
		t.Fatal("field 19 changed during rebuild")
	}
}

// integrityField returns the client-fingerprint hash block at 1.1.19.
func integrityField(req []byte) ([]byte, error) {
	meta, _, err := fieldBytesAt(req, 1, 0)
	if err != nil {
		return nil, err
	}
	ctx, _, err := fieldBytesAt(meta, 1, 0)
	if err != nil {
		return nil, err
	}
	v, _, err := fieldBytesAt(ctx, 19, 0)
	return v, err
}

// walkCheck verifies every length-delimited field in b is fully contained.
func walkCheck(t *testing.T, b []byte) {
	t.Helper()
	if err := checkWire(b, 0); err != nil {
		t.Fatalf("malformed protobuf: %v", err)
	}
}

func checkWire(b []byte, depth int) error {
	i := 0
	for i < len(b) {
		num, typ, n := protowire.ConsumeTag(b[i:])
		if n < 0 {
			return &wireError{depth, i, num, "bad tag"}
		}
		i += n
		switch typ {
		case protowire.BytesType:
			v, m := protowire.ConsumeBytes(b[i:])
			if m < 0 {
				return &wireError{depth, i, num, "length exceeds buffer"}
			}
			i += m
			// Recurse into what parses as a message; a plain string simply
			// fails the tag check and is treated as a leaf.
			if looksLikeMessage(v) {
				if err := checkWire(v, depth+1); err != nil {
					return err
				}
			}
		case protowire.VarintType:
			_, m := protowire.ConsumeVarint(b[i:])
			if m < 0 {
				return &wireError{depth, i, num, "bad varint"}
			}
			i += m
		case protowire.Fixed32Type:
			i += 4
		case protowire.Fixed64Type:
			i += 8
		default:
			return &wireError{depth, i, num, "unexpected wire type"}
		}
	}
	return nil
}

// looksLikeMessage reports whether b walks cleanly as a protobuf message.
func looksLikeMessage(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	i := 0
	for i < len(b) {
		_, typ, n := protowire.ConsumeTag(b[i:])
		if n <= 0 || typ > protowire.Fixed64Type {
			return false
		}
		i += n
		var adv int
		switch typ {
		case protowire.BytesType:
			_, adv = protowire.ConsumeBytes(b[i:])
		case protowire.VarintType:
			_, adv = protowire.ConsumeVarint(b[i:])
		case protowire.Fixed32Type:
			adv = 4
		case protowire.Fixed64Type:
			adv = 8
		}
		if adv <= 0 || i+adv > len(b) {
			return false
		}
		i += adv
	}
	return true
}

type wireError struct {
	depth int
	off   int
	field protowire.Number
	why   string
}

func (e *wireError) Error() string {
	return e.why + " at depth " + itoa(e.depth) + " offset " + itoa(e.off)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
