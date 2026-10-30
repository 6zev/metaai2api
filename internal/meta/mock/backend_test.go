package mock_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	backend "github.com/me/metaai2api/internal/backend"
	"github.com/me/metaai2api/internal/meta/mock"
)

var _ backend.ChatBackend = mock.New()

func TestName(t *testing.T) {
	if got := mock.New().Name(); got != "mock" {
		t.Fatalf("Name() = %q, want %q", got, "mock")
	}
}

func TestStreamDeltas(t *testing.T) {
	b := mock.New(mock.WithChunks(4), mock.WithSleep(time.Millisecond))

	var got []backend.Delta
	err := b.Stream(context.Background(), backend.ChatRequest{
		Messages: []backend.Message{{Role: "user", Content: "hello mock"}},
		Stream:   true,
	}, func(d backend.Delta) error {
		got = append(got, d)
		return nil
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if len(got) != 4 {
		t.Fatalf("deltas = %d, want 4", len(got))
	}
	for i, d := range got[:3] {
		if d.Done {
			t.Fatalf("delta %d prematurely Done", i)
		}
		if d.Text == "" {
			t.Fatalf("delta %d has empty text", i)
		}
	}
	last := got[len(got)-1]
	if !last.Done {
		t.Fatalf("last delta not Done: %+v", last)
	}

	var joined strings.Builder
	for _, d := range got {
		joined.WriteString(d.Text)
	}
	res, err := b.Complete(context.Background(), backend.ChatRequest{
		Messages: []backend.Message{{Role: "user", Content: "hello mock"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if joined.String() != res.Text {
		t.Fatalf("streamed text %q != complete text %q", joined.String(), res.Text)
	}
	if res.Text == "" {
		t.Fatal("Complete returned empty text")
	}
}

func TestStreamIsDeterministic(t *testing.T) {
	b := mock.New(mock.WithChunks(3), mock.WithSleep(0))
	req := backend.ChatRequest{Messages: []backend.Message{{Role: "user", Content: "same"}}}

	collect := func() string {
		var sb strings.Builder
		err := b.Stream(context.Background(), req, func(d backend.Delta) error {
			sb.WriteString(d.Text)
			return nil
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		return sb.String()
	}

	if a, c := collect(), collect(); a != c {
		t.Fatalf("stream not deterministic: %q vs %q", a, c)
	}
}

func TestCompleteConversationID(t *testing.T) {
	b := mock.New()
	res, err := b.Complete(context.Background(), backend.ChatRequest{
		Messages:       []backend.Message{{Role: "user", Content: "hi"}},
		ConversationID: "conv-1",
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.ConversationID != "conv-1" {
		t.Fatalf("ConversationID = %q, want conv-1", res.ConversationID)
	}

	res, err = b.Complete(context.Background(), backend.ChatRequest{
		Messages: []backend.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if res.ConversationID == "" {
		t.Fatal("ConversationID empty, want generated id")
	}
}

func TestCompleteEmptyMessages(t *testing.T) {
	res, err := mock.New().Complete(context.Background(), backend.ChatRequest{})
	if err != nil {
		t.Fatalf("Complete with no messages: %v", err)
	}
	if res.Text == "" {
		t.Fatal("Complete with no messages returned empty text")
	}
}

func TestCancelStopsStream(t *testing.T) {
	b := mock.New(mock.WithChunks(100), mock.WithSleep(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	start := time.Now()
	err := b.Stream(ctx, backend.ChatRequest{
		Messages: []backend.Message{{Role: "user", Content: "long"}},
	}, func(d backend.Delta) error {
		calls++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Stream err = %v, want context.Canceled", err)
	}
	if calls != 1 {
		t.Fatalf("emit calls = %d, want 1 (must stop after cancellation)", calls)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Stream took %v after cancel, want prompt return", elapsed)
	}
}

func TestEmitErrorStopsStream(t *testing.T) {
	b := mock.New(mock.WithChunks(10), mock.WithSleep(0))
	boom := errors.New("client gone")
	calls := 0
	err := b.Stream(context.Background(), backend.ChatRequest{
		Messages: []backend.Message{{Role: "user", Content: "x"}},
	}, func(d backend.Delta) error {
		calls++
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Stream err = %v, want boom", err)
	}
	if calls != 1 {
		t.Fatalf("emit calls = %d, want 1", calls)
	}
}

func TestCompleteHonorsCtx(t *testing.T) {
	b := mock.New(mock.WithChunks(50), mock.WithSleep(20*time.Millisecond))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := b.Complete(ctx, backend.ChatRequest{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete err = %v, want context.Canceled", err)
	}
}
