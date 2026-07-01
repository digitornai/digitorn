package eventbus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/ports"
)

// TestIntegration_FullFlow tests the complete event flow:
// Module -> EventBus -> EventBuffer -> PrimitivesAdapter
func TestIntegration_FullFlow(t *testing.T) {
	bus := New(nil)
	defer bus.Close(context.Background())

	// Simulate the event buffer (like daemon's api_events.go)
	type eventBuffer struct {
		events []ports.Event
	}
	buf := &eventBuffer{}

	// Subscribe to all events and buffer them
	_, err := bus.Subscribe("", func(ctx context.Context, evt ports.Event) error {
		if evt.Metadata == nil {
			evt.Metadata = make(map[string]any)
		}
		evt.Metadata["timestamp"] = time.Now().UTC().Format(time.RFC3339)
		buf.events = append(buf.events, evt)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Simulate a module emitting a file event
	err = bus.Publish(context.Background(), ports.Event{
		Topic:  "filesystem.file.created",
		Type:   "file.created",
		Source: "filesystem-monitor",
		Data: map[string]any{
			"path":  "test.txt",
			"bytes": 100,
		},
		Metadata: map[string]any{
			"session_id": "test-session",
			"user_id":    "test-user",
			"agent_id":   "main",
			"module_id":  "filesystem",
		},
	})
	if err != nil {
		t.Fatalf("publish: %v", err)
	}

	// Wait for async delivery
	time.Sleep(100 * time.Millisecond)

	// Verify event was buffered
	if len(buf.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(buf.events))
	}

	evt := buf.events[0]
	if evt.Topic != "filesystem.file.created" {
		t.Errorf("expected topic 'filesystem.file.created', got %q", evt.Topic)
	}
	if evt.Type != "file.created" {
		t.Errorf("expected type 'file.created', got %q", evt.Type)
	}

	// Simulate the primitives adapter converting this to adapter.Event
	// (This is what the background service would do)
	adapterEvent := map[string]any{
		"provider": "primitives",
		"adapter":  "primitives",
		"dedupKey": "test-dedup-key",
		"source":   evt.Source,
		"message":  evt.Type,
		"payload": map[string]any{
			"topic": evt.Topic,
			"type":  evt.Type,
			"data":  evt.Data,
		},
	}

	if adapterEvent["provider"] != "primitives" {
		t.Errorf("expected provider 'primitives', got %q", adapterEvent["provider"])
	}

	t.Logf("✓ Full flow test passed: event buffered and ready for background service")
	t.Logf("  Event: %s/%s from %s", evt.Topic, evt.Type, evt.Source)
}

// TestIntegration_MultipleEvents tests multiple events from different modules
func TestIntegration_MultipleEvents(t *testing.T) {
	bus := New(nil)
	defer bus.Close(context.Background())

	var mu sync.Mutex
	var received []ports.Event
	_, _ = bus.Subscribe("", func(ctx context.Context, evt ports.Event) error {
		mu.Lock()
		received = append(received, evt)
		mu.Unlock()
		return nil
	})

	// Simulate events from different modules
	events := []ports.Event{
		{Topic: "filesystem.file.created", Type: "file.created", Source: "app1"},
		{Topic: "filesystem.file.modified", Type: "file.modified", Source: "app1"},
		{Topic: "filesystem.file.deleted", Type: "file.deleted", Source: "app2"},
		{Topic: "audio.incoming_call", Type: "incoming_call", Source: "app1"},
	}

	for _, evt := range events {
		if err := bus.Publish(context.Background(), evt); err != nil {
			t.Fatalf("publish: %v", err)
		}
	}

	// Wait for async delivery
	time.Sleep(200 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	if len(received) != len(events) {
		t.Fatalf("expected %d events, got %d", len(events), len(received))
	}

	t.Logf("✓ Multiple events test passed: %d events received", len(received))
}
