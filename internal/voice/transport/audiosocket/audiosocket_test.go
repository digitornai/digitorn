package audiosocket

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/voice"
)

func writeFrame(t *testing.T, conn net.Conn, kind byte, payload []byte) {
	t.Helper()
	var hdr [3]byte
	hdr[0] = kind
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(payload)))
	if _, err := conn.Write(hdr[:]); err != nil {
		t.Fatal(err)
	}
	if len(payload) > 0 {
		if _, err := conn.Write(payload); err != nil {
			t.Fatal(err)
		}
	}
}

func readFrame(t *testing.T, conn net.Conn) (byte, []byte) {
	t.Helper()
	var hdr [3]byte
	if _, err := io.ReadFull(conn, hdr[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := binary.BigEndian.Uint16(hdr[1:])
	p := make([]byte, n)
	if n > 0 {
		if _, err := io.ReadFull(conn, p); err != nil {
			t.Fatalf("read payload: %v", err)
		}
	}
	return hdr[0], p
}

// TestAudioSocket_RoundTrip proves a real transport: a fake Asterisk sends a call
// UUID + audio; the handler (an echo) reads decoded Frames and writes Frames back;
// the peer receives them re-encoded as AudioSocket audio.
func TestAudioSocket_RoundTrip(t *testing.T) {
	tr := New("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	gotCall := make(chan voice.Call, 1)
	handler := func(_ context.Context, c voice.Call) {
		gotCall <- c
		for f := range c.In() {
			c.Out() <- f // echo
		}
	}
	go func() { _ = tr.Serve(ctx, handler) }()

	// Wait for the listener to bind.
	deadline := time.Now().Add(2 * time.Second)
	for tr.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	conn, err := net.Dial("tcp", tr.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send the call UUID, then one audio frame (160 samples = 20 ms).
	id := make([]byte, 16)
	for i := range id {
		id[i] = byte(i + 1)
	}
	writeFrame(t, conn, kindID, id)

	samples := make([]int16, 160)
	for i := range samples {
		samples[i] = int16(i * 10)
	}
	writeFrame(t, conn, kindAudio, encode(voice.Frame{Samples: samples, Rate: 8000}))

	// The handler echoes it back as an audio frame.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	kind, payload := readFrame(t, conn)
	if kind != kindAudio {
		t.Fatalf("expected audio frame back, got kind 0x%x", kind)
	}
	out := decode(payload)
	if len(out.Samples) != 160 || out.Samples[5] != 50 {
		t.Fatalf("round-tripped audio wrong: %d samples, [5]=%d", len(out.Samples), out.Samples[5])
	}

	// The call exposes the UUID + caller.
	select {
	case c := <-gotCall:
		if c.ID() != hex.EncodeToString(id) {
			t.Fatalf("call id = %q, want %q", c.ID(), hex.EncodeToString(id))
		}
		if c.Caller() == "" {
			t.Fatal("caller empty")
		}
	case <-time.After(time.Second):
		t.Fatal("handler never received the call")
	}
}

// TestAudioSocket_Hangup proves an inbound hangup closes the call's In channel.
func TestAudioSocket_Hangup(t *testing.T) {
	tr := New("127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})
	handler := func(_ context.Context, c voice.Call) {
		for range c.In() {
		}
		close(done) // In closed → call ended
	}
	go func() { _ = tr.Serve(ctx, handler) }()
	deadline := time.Now().Add(2 * time.Second)
	for tr.Addr() == "127.0.0.1:0" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	conn, err := net.Dial("tcp", tr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	writeFrame(t, conn, kindAudio, encode(voice.Frame{Samples: make([]int16, 160), Rate: 8000}))
	writeFrame(t, conn, kindHangup, nil)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("hangup did not end the call")
	}
	conn.Close()
}
