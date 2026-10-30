// Package backend defines the seam between the OpenAI-compatible HTTP layer
// and the upstream chat implementation (mock or DGW).
//
// The package holds types only: no logic, no imports outside the standard
// library, and no dependency on any other internal package. Implementations
// (internal/meta/mock, internal/meta/dgw) depend on this package, never the
// reverse.
package backend

import (
	"context"
	"errors"
)

// Typed errors returned by implementations. Callers match with errors.Is, so
// implementations are expected to wrap them rather than replace the value.
var (
	// ErrUpstreamProtocol reports a malformed or unexpected response from the
	// upstream service (bad frame, unparsable payload, missing field).
	ErrUpstreamProtocol = errors.New("backend: upstream protocol error")

	// ErrSessionExpired reports that the supplied meta.ai cookie or access
	// token is no longer valid and the operator must refresh it.
	ErrSessionExpired = errors.New("backend: session expired")

	// ErrNotImplemented reports a capability the backend does not provide.
	ErrNotImplemented = errors.New("backend: not implemented")

	// ErrUnavailable reports that the backend cannot run in this build or
	// configuration (for example a missing golden vector or WASM codec).
	ErrUnavailable = errors.New("backend: unavailable")
)

// Message is one entry in a conversation sent to a backend.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is a single completion request.
type ChatRequest struct {
	Messages       []Message
	Model          string
	Stream         bool
	ConversationID string
}

// ChatResult is the complete answer for a non-streaming request.
type ChatResult struct {
	Text           string
	ConversationID string
}

// Delta is one streamed increment. Text carries the fragment; Done marks the
// final delta; Err carries a terminal error. A terminal Delta may set both
// Done and Err.
type Delta struct {
	Text string
	Done bool
	Err  error
}

// ChatBackend is the interface every upstream implementation satisfies.
//
// Complete returns the whole answer. Stream delivers increments to emit,
// which may be called zero or more times; an error returned by emit stops the
// stream and is returned to the caller. Implementations must honor ctx
// cancellation.
type ChatBackend interface {
	Complete(ctx context.Context, req ChatRequest) (ChatResult, error)
	Stream(ctx context.Context, req ChatRequest, emit func(Delta) error) error
	Name() string
}
