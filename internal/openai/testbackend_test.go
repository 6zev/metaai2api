package openai_test

import (
	"context"
	"errors"
	"sync"

	backend "github.com/me/metaai2api/internal/backend"
)

// gateBackend is a ChatBackend whose Stream blocks after the first delta until
// release is closed. It lets a test observe the first SSE chunk arriving while
// the upstream is still producing, which is the only way to prove the handler
// flushes per delta instead of buffering the whole stream.
type gateBackend struct {
	release  chan struct{}
	first    string
	rest     []string
	requests chan backend.ChatRequest

	mu       sync.Mutex
	err      error
	complete string
}

func newGateBackend(first string, rest ...string) *gateBackend {
	return &gateBackend{
		release:  make(chan struct{}),
		first:    first,
		rest:     rest,
		requests: make(chan backend.ChatRequest, 8),
	}
}

func (b *gateBackend) Name() string { return "gate" }

// setErr makes Complete fail with err (Stream stays gated).
func (b *gateBackend) setErr(err error) {
	b.mu.Lock()
	b.err = err
	b.mu.Unlock()
}

func (b *gateBackend) Complete(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	b.record(req)
	b.mu.Lock()
	err, text := b.err, b.complete
	b.mu.Unlock()
	if err != nil {
		return backend.ChatResult{}, err
	}
	if text == "" {
		text = b.first
		for _, s := range b.rest {
			text += s
		}
	}
	return backend.ChatResult{Text: text, ConversationID: "gate-conv"}, nil
}

func (b *gateBackend) Stream(ctx context.Context, req backend.ChatRequest, emit func(backend.Delta) error) error {
	b.record(req)
	if err := emit(backend.Delta{Text: b.first}); err != nil {
		return err
	}
	select {
	case <-b.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	for i, s := range b.rest {
		if err := emit(backend.Delta{Text: s, Done: i == len(b.rest)-1}); err != nil {
			return err
		}
	}
	if len(b.rest) == 0 {
		return emit(backend.Delta{Done: true})
	}
	return nil
}

// record reports the request to the test without ever blocking.
func (b *gateBackend) record(req backend.ChatRequest) {
	select {
	case b.requests <- req:
	default:
	}
}

// emitErrBackend fails Stream with err after the first delta.
type emitErrBackend struct {
	err error
}

func (b *emitErrBackend) Name() string { return "err" }

func (b *emitErrBackend) Complete(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	return backend.ChatResult{}, b.err
}

func (b *emitErrBackend) Stream(ctx context.Context, req backend.ChatRequest, emit func(backend.Delta) error) error {
	if err := emit(backend.Delta{Text: "partial"}); err != nil {
		return err
	}
	return b.err
}

var errGateTest = errors.New("gate: upstream exploded")
