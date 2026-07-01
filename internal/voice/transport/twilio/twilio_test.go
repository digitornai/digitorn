package twilio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/digitornai/digitorn/internal/voice"
	"github.com/digitornai/digitorn/internal/voice/codec"
)

// startServer runs the transport on an ephemeral port and returns a connected
// fake-Twilio client plus the channel on which the accepted call arrives.
func startServer(t *testing.T) (*websocket.Conn, <-chan voice.Call, context.CancelFunc) {
	t.Helper()
	tr := New("127.0.0.1:0", "")
	ctx, cancel := context.WithCancel(context.Background())
	calls := make(chan voice.Call, 1)
	go func() {
		_ = tr.Serve(ctx, func(cctx context.Context, c voice.Call) {
			calls <- c
			<-cctx.Done() // keep the call open until the test cancels
		})
	}()
	// Wait for the listener to bind.
	deadline := time.Now().Add(2 * time.Second)
	for tr.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	conn, _, err := websocket.DefaultDialer.Dial("ws://"+tr.Addr()+"/stream", nil)
	if err != nil {
		cancel()
		t.Fatalf("dial: %v", err)
	}
	return conn, calls, cancel
}

func sendJSON(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	if err := c.WriteJSON(v); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The full Twilio handshake : connected → start (ids) → media in (μ-law→PCM16),
// media out (PCM16→μ-law), clear on demand, stop closes In.
func TestTwilio_FullStream(t *testing.T) {
	conn, calls, cancel := startServer(t)
	defer cancel()
	defer conn.Close()

	sendJSON(t, conn, map[string]any{"event": "connected", "protocol": "Call"})
	sendJSON(t, conn, map[string]any{
		"event":     "start",
		"streamSid": "MZ123",
		"start":     map[string]any{"streamSid": "MZ123", "callSid": "CA456"},
	})

	var call voice.Call
	select {
	case call = <-calls:
	case <-time.After(2 * time.Second):
		t.Fatal("handler not invoked after start")
	}
	if call.ID() != "CA456" {
		t.Errorf("call id = %q, want CA456", call.ID())
	}

	// Inbound media : a known PCM16 pattern, μ-law'd + base64'd like Twilio does.
	want := []int16{0, 1000, -1000, 12000, -12000, 30000}
	sendJSON(t, conn, map[string]any{
		"event": "media",
		"media": map[string]any{"track": "inbound", "payload": base64.StdEncoding.EncodeToString(codec.MuLawEncodeAll(want))},
	})
	select {
	case f := <-call.In():
		if f.Rate != 8000 || len(f.Samples) != len(want) {
			t.Fatalf("frame shape: rate=%d n=%d", f.Rate, len(f.Samples))
		}
		for i := range want {
			d := int32(f.Samples[i]) - int32(want[i])
			if d < 0 {
				d = -d
			}
			if d > 1024 { // μ-law quantisation tolerance
				t.Fatalf("sample %d: got %d want ≈%d", i, f.Samples[i], want[i])
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound media not delivered")
	}

	// Outbound media : engine audio must reach the client as a media event
	// addressed to the stream.
	call.Out() <- voice.Frame{Samples: []int16{500, -500, 0, 20000}, Rate: 8000}
	var out struct {
		Event     string `json:"event"`
		StreamSid string `json:"streamSid"`
		Media     struct {
			Payload string `json:"payload"`
		} `json:"media"`
	}
	if err := conn.ReadJSON(&out); err != nil {
		t.Fatalf("read outbound: %v", err)
	}
	if out.Event != "media" || out.StreamSid != "MZ123" || out.Media.Payload == "" {
		t.Fatalf("outbound media wrong: %+v", out)
	}
	raw, _ := base64.StdEncoding.DecodeString(out.Media.Payload)
	dec := codec.MuLawDecodeAll(raw)
	if len(dec) != 4 || dec[3] < 15000 { // round-trip sanity on the loud sample
		t.Fatalf("outbound payload decode wrong: %v", dec)
	}

	// Barge-in : ClearPlayback must emit the clear event.
	call.(voice.PlaybackClearer).ClearPlayback()
	var clr map[string]any
	if err := conn.ReadJSON(&clr); err != nil {
		t.Fatalf("read clear: %v", err)
	}
	if clr["event"] != "clear" || clr["streamSid"] != "MZ123" {
		t.Fatalf("clear event wrong: %v", clr)
	}

	// stop ends the inbound stream : In closes.
	sendJSON(t, conn, map[string]any{"event": "stop", "streamSid": "MZ123"})
	select {
	case _, ok := <-call.In():
		if ok {
			t.Fatal("In must close after stop")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("In not closed after stop")
	}
}

// Bidirectional streams echo our own audio back on track "outbound" — it must
// never feed the engine (it would hear itself).
func TestTwilio_OutboundTrackIgnored(t *testing.T) {
	conn, calls, cancel := startServer(t)
	defer cancel()
	defer conn.Close()

	sendJSON(t, conn, map[string]any{"event": "start", "start": map[string]any{"streamSid": "MZ1", "callSid": "CA1"}})
	call := <-calls

	echo := base64.StdEncoding.EncodeToString(codec.MuLawEncodeAll([]int16{9000}))
	sendJSON(t, conn, map[string]any{"event": "media", "media": map[string]any{"track": "outbound", "payload": echo}})
	inb := base64.StdEncoding.EncodeToString(codec.MuLawEncodeAll([]int16{-7000}))
	sendJSON(t, conn, map[string]any{"event": "media", "media": map[string]any{"track": "inbound", "payload": inb}})

	select {
	case f := <-call.In():
		if f.Samples[0] > 0 {
			t.Fatalf("echo (outbound track) leaked into the engine: %v", f.Samples)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("inbound media not delivered")
	}
}

// A stream that stops before "start" never reaches the handler.
func TestTwilio_StopBeforeStart(t *testing.T) {
	conn, calls, cancel := startServer(t)
	defer cancel()
	defer conn.Close()

	sendJSON(t, conn, map[string]any{"event": "connected"})
	sendJSON(t, conn, map[string]any{"event": "stop"})
	select {
	case <-calls:
		t.Fatal("handler must not run without a start event")
	case <-time.After(300 * time.Millisecond):
	}
}

// JSON marshal sanity for the inbound event union.
func TestInboundEventDecode(t *testing.T) {
	raw := `{"event":"start","sequenceNumber":"1","start":{"streamSid":"MZ9","callSid":"CA9","mediaFormat":{"encoding":"audio/x-mulaw","sampleRate":8000,"channels":1}},"streamSid":"MZ9"}`
	var ev inboundEvent
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Event != "start" || ev.Start.StreamSid != "MZ9" || ev.Start.CallSid != "CA9" {
		t.Fatalf("decode wrong: %+v", ev)
	}
}
