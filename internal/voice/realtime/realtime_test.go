package realtime

import (
	"context"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/mbathepaul/digitorn/internal/voice"
)

type fakeConn struct {
	mu     sync.Mutex
	sent   []map[string]any
	events chan map[string]any
}

func (c *fakeConn) Send(ev map[string]any) error {
	c.mu.Lock()
	c.sent = append(c.sent, ev)
	c.mu.Unlock()
	return nil
}
func (c *fakeConn) Events() <-chan map[string]any { return c.events }
func (c *fakeConn) Close() error                  { return nil }

func (c *fakeConn) has(t *testing.T, typ string) bool {
	t.Helper()
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.sent {
		if e["type"] == typ {
			return true
		}
	}
	return false
}

type fakeTools struct {
	specs    []ToolSpec
	mu       sync.Mutex
	executed string
}

func (t *fakeTools) Specs() []ToolSpec { return t.specs }
func (t *fakeTools) Execute(_ context.Context, _ /*callID*/, name, _ string) (string, error) {
	t.mu.Lock()
	t.executed = name
	t.mu.Unlock()
	return `{"temp":21}`, nil
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		if cond() {
			return
		}
		select {
		case <-deadline:
			t.Fatal(msg)
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestRealtime_Brain proves the realtime session's full control flow: config with the
// gated toolset + turn_detection off, audio append, VAD-driven commit, audio playback,
// and — the key part — function-call interception routed through the gated executor.
func TestRealtime_Brain(t *testing.T) {
	conn := &fakeConn{events: make(chan map[string]any, 16)}
	tools := &fakeTools{specs: []ToolSpec{{Name: "get_weather", Description: "weather"}}}
	eng := New(func(context.Context, voice.SessionOpts) (Conn, error) { return conn, nil }, tools, "gpt-realtime", "alloy")

	sess, err := eng.Session(context.Background(), voice.SessionOpts{SampleRate: 24000, Context: "spoken"})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	// session.update sent first, with the toolset + turn_detection disabled.
	conn.mu.Lock()
	first := conn.sent[0]
	conn.mu.Unlock()
	if first["type"] != "session.update" {
		t.Fatalf("first event = %v", first["type"])
	}
	su := first["session"].(map[string]any)
	if su["turn_detection"] != nil {
		t.Fatal("turn_detection should be nil (our VAD drives turns)")
	}
	if _, ok := su["tools"]; !ok {
		t.Fatal("gated toolset not sent to the model")
	}

	// Drain Out + Events so the session never blocks.
	gotFrame := make(chan struct{}, 1)
	gotSpeaking := make(chan struct{}, 1)
	go func() {
		for range sess.Out() {
			select {
			case gotFrame <- struct{}{}:
			default:
			}
		}
	}()
	go func() {
		for e := range sess.Events() {
			if e.Kind == voice.EvSpeakingStart {
				select {
				case gotSpeaking <- struct{}{}:
				default:
				}
			}
		}
	}()

	// Audio in → append.
	sess.Audio() <- voice.Frame{Samples: []int16{1, 2, 3}, Rate: 24000}
	waitFor(t, func() bool { return conn.has(t, "input_audio_buffer.append") }, "audio not appended")

	// VAD endpoint → commit + response.create.
	sess.Commit()
	waitFor(t, func() bool { return conn.has(t, "input_audio_buffer.commit") && conn.has(t, "response.create") }, "commit/response not sent")

	// Provider streams audio → outbound frame + speaking-start.
	conn.events <- map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte{0x10, 0x00})}
	waitFor(t, func() bool {
		select {
		case <-gotFrame:
			return true
		default:
			return false
		}
	}, "no outbound audio frame")
	waitFor(t, func() bool {
		select {
		case <-gotSpeaking:
			return true
		default:
			return false
		}
	}, "no EvSpeakingStart")

	// Function call → gated execution → function_call_output + response.create.
	conn.events <- map[string]any{"type": "response.function_call_arguments.done", "call_id": "c1", "name": "get_weather", "arguments": "{}"}
	waitFor(t, func() bool {
		tools.mu.Lock()
		defer tools.mu.Unlock()
		return tools.executed == "get_weather"
	}, "tool not executed through the gate")
	waitFor(t, func() bool { return conn.has(t, "conversation.item.create") }, "function_call_output not sent back")

	// Barge-in → response.cancel.
	sess.Cancel()
	waitFor(t, func() bool { return conn.has(t, "response.cancel") }, "barge-in did not cancel the response")
}
