package updater

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestPollStopsOnCancel proves the optional poller runs zero or more ticks and
// returns promptly when its context is cancelled.
func TestPollStopsOnCancel(t *testing.T) {
	var ticks atomic.Int64
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		Poll(ctx, time.Millisecond, func(context.Context) {
			if ticks.Add(1) >= 3 {
				cancel()
			}
		})
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Poll did not return after context cancellation")
	}
	if got := ticks.Load(); got < 3 {
		t.Fatalf("ticks = %d, want at least 3", got)
	}
}

// TestPollZeroIntervalDoesNothing proves a non-positive interval disables the
// poller rather than spinning.
func TestPollZeroIntervalDoesNothing(t *testing.T) {
	var calls atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	Poll(ctx, 0, func(context.Context) { calls.Add(1) })
	if calls.Load() != 0 {
		t.Fatalf("calls = %d, want 0 for a zero interval", calls.Load())
	}
}
