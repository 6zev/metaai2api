// Package mock provides a deterministic backend.ChatBackend implementation.
//
// It is the default backend: it makes the server useful without credentials
// and gives the HTTP layer a stable upstream for tests. It deliberately knows
// nothing about the meta.ai capture; importing internal/meta from here would
// create an import cycle.
package mock

import (
	"context"
	"fmt"
	"strings"
	"time"

	backend "github.com/me/metaai2api/internal/backend"
)

const (
	defaultChunks = 8
	defaultSleep  = 10 * time.Millisecond
)

// Option configures a Backend.
type Option func(*Backend)

// WithChunks sets how many deltas Stream emits for a given request.
func WithChunks(n int) Option {
	return func(b *Backend) {
		if n > 0 {
			b.chunks = n
		}
	}
}

// WithSleep sets the pause between deltas.
func WithSleep(d time.Duration) Option {
	return func(b *Backend) {
		if d >= 0 {
			b.sleep = d
		}
	}
}

// Backend is a deterministic chat backend.
type Backend struct {
	chunks int
	sleep  time.Duration
}

// New returns a mock backend. The default emits 8 deltas 10ms apart.
func New(opts ...Option) *Backend {
	b := &Backend{chunks: defaultChunks, sleep: defaultSleep}
	for _, opt := range opts {
		opt(b)
	}
	return b
}

// Name implements backend.ChatBackend.
func (b *Backend) Name() string { return "mock" }

// Complete drains Stream and returns the concatenated answer.
func (b *Backend) Complete(ctx context.Context, req backend.ChatRequest) (backend.ChatResult, error) {
	var sb strings.Builder
	err := b.Stream(ctx, req, func(d backend.Delta) error {
		sb.WriteString(d.Text)
		return nil
	})
	if err != nil {
		return backend.ChatResult{}, err
	}
	return backend.ChatResult{
		Text:           sb.String(),
		ConversationID: conversationID(req),
	}, nil
}

// Stream emits chunks of a deterministic echo answer. The final delta has
// Done set. Cancellation is checked before every sleep and emit, and an error
// from emit is returned as-is.
func (b *Backend) Stream(ctx context.Context, req backend.ChatRequest, emit func(backend.Delta) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	answer := answer(req)
	chunks := split(answer, b.chunks)

	for i, chunk := range chunks {
		if err := b.pause(ctx); err != nil {
			return err
		}
		d := backend.Delta{Text: chunk, Done: i == len(chunks)-1}
		if err := emit(d); err != nil {
			return err
		}
	}

	if len(chunks) == 0 {
		if err := emit(backend.Delta{Done: true}); err != nil {
			return err
		}
	}
	return nil
}

func (b *Backend) pause(ctx context.Context) error {
	if b.sleep <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(b.sleep)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func conversationID(req backend.ChatRequest) string {
	if req.ConversationID != "" {
		return req.ConversationID
	}
	return "mock-" + promptHash(req)
}

func answer(req backend.ChatRequest) string {
	if len(req.Messages) == 0 {
		return "mock backend: no messages received"
	}
	last := req.Messages[len(req.Messages)-1]
	model := req.Model
	if model == "" {
		model = "mock"
	}
	return fmt.Sprintf("mock response [%s]: %s", model, last.Content)
}

func promptHash(req backend.ChatRequest) string {
	var h uint32 = 2166136261
	for _, m := range req.Messages {
		for _, part := range []string{m.Role, m.Content} {
			for i := 0; i < len(part); i++ {
				h ^= uint32(part[i])
				h *= 16777619
			}
		}
	}
	return fmt.Sprintf("%08x", h)
}

func split(s string, n int) []string {
	if s == "" || n <= 0 {
		return nil
	}
	if n > len(s) {
		n = len(s)
	}
	out := make([]string, 0, n)
	size := (len(s) + n - 1) / n
	for start := 0; start < len(s); start += size {
		end := start + size
		if end > len(s) {
			end = len(s)
		}
		out = append(out, s[start:end])
	}
	return out
}
