package eventbus

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/ports"
)

func TestPublishSubscribe(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	received := make(chan ports.Event, 1)

	_, err := bus.Subscribe("test.topic", func(ctx context.Context, evt ports.Event) error {
		received <- evt
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = bus.Publish(context.Background(), ports.Event{
		Topic:  "test.topic",
		Type:   "test.event",
		Source: "test-source",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case evt := <-received:
		if evt.Type != "test.event" {
			t.Errorf("expected type 'test.event', got %q", evt.Type)
		}
		if evt.Source != "test-source" {
			t.Errorf("expected source 'test-source', got %q", evt.Source)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event")
	}
}

func TestMultipleSubscribers(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	var mu sync.Mutex
	received := make(map[int]bool)

	for i := 0; i < 3; i++ {
		idx := i
		_, err := bus.Subscribe("multi.topic", func(ctx context.Context, evt ports.Event) error {
			mu.Lock()
			received[idx] = true
			mu.Unlock()
			return nil
		})
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
	}

	err := bus.Publish(context.Background(), ports.Event{
		Topic: "multi.topic",
		Type:  "multi.event",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != 3 {
		t.Errorf("expected 3 subscribers to receive, got %d", len(received))
	}
}

func TestUnsubscribe(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	received := make(chan ports.Event, 1)

	unsub, err := bus.Subscribe("unsub.topic", func(ctx context.Context, evt ports.Event) error {
		received <- evt
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Unsubscribe
	unsub()

	err = bus.Publish(context.Background(), ports.Event{
		Topic: "unsub.topic",
		Type:  "unsub.event",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-received:
		t.Error("should not have received event after unsubscribe")
	case <-time.After(100 * time.Millisecond):
		// Expected: no event received
	}
}

func TestPublishAfterClose(t *testing.T) {
	bus := New(slog.Default())
	bus.Close(context.Background())

	err := bus.Publish(context.Background(), ports.Event{
		Topic: "closed.topic",
		Type:  "closed.event",
	})
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("expected ErrBusClosed, got %v", err)
	}
}

func TestSubscribeAfterClose(t *testing.T) {
	bus := New(slog.Default())
	bus.Close(context.Background())

	_, err := bus.Subscribe("closed.topic", func(ctx context.Context, evt ports.Event) error {
		return nil
	})
	if !errors.Is(err, ErrBusClosed) {
		t.Errorf("expected ErrBusClosed, got %v", err)
	}
}

func TestHandlerErrorDoesNotBlockOthers(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	var mu sync.Mutex
	received := make(map[int]bool)

	// First handler errors
	_, err := bus.Subscribe("error.topic", func(ctx context.Context, evt ports.Event) error {
		return errors.New("handler error")
	})
	if err != nil {
		t.Fatalf("subscribe 0: %v", err)
	}

	// Second handler succeeds
	_, err = bus.Subscribe("error.topic", func(ctx context.Context, evt ports.Event) error {
		mu.Lock()
		received[1] = true
		mu.Unlock()
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe 1: %v", err)
	}

	err = bus.Publish(context.Background(), ports.Event{
		Topic: "error.topic",
		Type:  "error.event",
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if !received[1] {
		t.Error("second handler should have received event despite first handler error")
	}
}

func TestDifferentTopics(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	receivedA := make(chan ports.Event, 1)
	receivedB := make(chan ports.Event, 1)

	_, err := bus.Subscribe("topic.a", func(ctx context.Context, evt ports.Event) error {
		receivedA <- evt
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe A: %v", err)
	}

	_, err = bus.Subscribe("topic.b", func(ctx context.Context, evt ports.Event) error {
		receivedB <- evt
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe B: %v", err)
	}

	// Publish to topic A
	err = bus.Publish(context.Background(), ports.Event{
		Topic: "topic.a",
		Type:  "event.a",
	})
	if err != nil {
		t.Fatalf("publish A: %v", err)
	}

	select {
	case evt := <-receivedA:
		if evt.Type != "event.a" {
			t.Errorf("expected type 'event.a', got %q", evt.Type)
		}
	case <-time.After(time.Second):
		t.Fatal("timeout waiting for event A")
	}

	// topic B should not have received
	select {
	case <-receivedB:
		t.Error("topic B should not have received event for topic A")
	case <-time.After(100 * time.Millisecond):
		// Expected
	}
}

func TestBusFullDropsEvent(t *testing.T) {
	// Create a bus with a very small channel
	bus := &Bus{
		subscribers: make(map[string][]ports.EventHandlerFunc),
		logger:      slog.Default(),
		events:      make(chan ports.Event, 1), // Only 1 slot
		workers:     1,
	}
	bus.startWorkers()
	defer bus.Close(context.Background())

	// Subscribe with a slow handler
	_, _ = bus.Subscribe("slow.topic", func(ctx context.Context, evt ports.Event) error {
		time.Sleep(100 * time.Millisecond)
		return nil
	})

	// Fill the channel
	_ = bus.Publish(context.Background(), ports.Event{Topic: "slow.topic", Type: "event1"})

	// This should be dropped (channel full)
	err := bus.Publish(context.Background(), ports.Event{Topic: "slow.topic", Type: "event2"})
	if !errors.Is(err, ErrBusFull) {
		t.Errorf("expected ErrBusFull, got %v", err)
	}

	// Verify metrics
	stats := bus.Stats()
	if stats["dropped"] == 0 {
		t.Error("expected dropped > 0")
	}
}

func TestStats(t *testing.T) {
	bus := New(slog.Default())
	defer bus.Close(context.Background())

	_, _ = bus.Subscribe("stats.topic", func(ctx context.Context, evt ports.Event) error {
		return nil
	})

	_ = bus.Publish(context.Background(), ports.Event{Topic: "stats.topic", Type: "event1"})
	_ = bus.Publish(context.Background(), ports.Event{Topic: "stats.topic", Type: "event2"})

	time.Sleep(100 * time.Millisecond)

	stats := bus.Stats()
	if stats["published"] != 2 {
		t.Errorf("expected published=2, got %d", stats["published"])
	}
	if stats["delivered"] != 2 {
		t.Errorf("expected delivered=2, got %d", stats["delivered"])
	}
}
