package backend_test

import (
	"context"
	"errors"
	"testing"

	backend "github.com/me/metaai2api/internal/backend"
)

type stubBackend struct {
	result backend.ChatResult
	err    error
	deltas []backend.Delta
}

func (s *stubBackend) Complete(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	return s.result, s.err
}

func (s *stubBackend) Stream(ctx context.Context, req backend.ChatRequest, emit func(backend.Delta) error) error {
	for _, d := range s.deltas {
		if err := emit(d); err != nil {
			return err
		}
	}
	return s.err
}

func (s *stubBackend) Name() string { return "stub" }

var _ backend.ChatBackend = (*stubBackend)(nil)

func TestChatBackendContract(t *testing.T) {
	var b backend.ChatBackend = &stubBackend{
		result: backend.ChatResult{Text: "hello", ConversationID: "c1"},
		deltas: []backend.Delta{{Text: "he"}, {Text: "llo", Done: true}},
	}

	if got := b.Name(); got != "stub" {
		t.Fatalf("Name() = %q, want %q", got, "stub")
	}

	res, err := b.Complete(context.Background(), backend.ChatRequest{
		Messages:       []backend.Message{{Role: "user", Content: "hi"}},
		Model:          "meta-think-fast",
		Stream:         false,
		ConversationID: "c1",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.Text != "hello" || res.ConversationID != "c1" {
		t.Fatalf("Complete result = %+v, want Text=hello ConversationID=c1", res)
	}

	var got []backend.Delta
	err = b.Stream(context.Background(), backend.ChatRequest{Stream: true}, func(d backend.Delta) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 2 || got[0].Text != "he" || !got[1].Done {
		t.Fatalf("Stream deltas = %+v, want two deltas ending Done", got)
	}
}

func TestChatRequestZeroValueIsUsable(t *testing.T) {
	var req backend.ChatRequest
	if req.Stream || len(req.Messages) != 0 || req.ConversationID != "" {
		t.Fatalf("zero ChatRequest = %+v, want zero fields", req)
	}
}

func TestDeltaCarriesError(t *testing.T) {
	boom := errors.New("boom")
	d := backend.Delta{Err: boom}
	if !errors.Is(d.Err, boom) {
		t.Fatalf("Delta.Err = %v, want boom", d.Err)
	}
	if d.Done {
		t.Fatal("Delta.Err alone must not imply Done")
	}
}

func TestTypedErrorsAreDistinct(t *testing.T) {
	errs := []error{
		backend.ErrUpstreamProtocol,
		backend.ErrSessionExpired,
		backend.ErrNotImplemented,
		backend.ErrUnavailable,
	}
	for i := range errs {
		if errs[i] == nil {
			t.Fatalf("error %d is nil", i)
		}
		if errs[i].Error() == "" {
			t.Fatalf("error %d has empty message", i)
		}
		for j := range errs {
			if i != j && errors.Is(errs[i], errs[j]) {
				t.Fatalf("errors %d and %d must not match: %v vs %v", i, j, errs[i], errs[j])
			}
		}
	}
}

func TestTypedErrorsSurviveWrapping(t *testing.T) {
	wrapped := errors.Join(errors.New("dial: refused"), backend.ErrUnavailable)
	if !errors.Is(wrapped, backend.ErrUnavailable) {
		t.Fatalf("errors.Is(join, ErrUnavailable) = false, want true")
	}

	outer := &wrapErr{err: backend.ErrSessionExpired}
	if !errors.Is(outer, backend.ErrSessionExpired) {
		t.Fatalf("errors.Is(wrapped, ErrSessionExpired) = false, want true")
	}
}

type wrapErr struct{ err error }

func (w *wrapErr) Error() string { return "upstream: " + w.err.Error() }
func (w *wrapErr) Unwrap() error { return w.err }
