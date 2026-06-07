package ws

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mbathepaul/digitorn/internal/voice"
)

// TestWSTransport_Roundtrip proves a WS connection becomes a Call: inbound binary PCM
// reaches Call.In decoded at the negotiated rate, and Call.Out frames reach the client
// as binary PCM. No base64 — raw bytes both ways.
func TestWSTransport_Roundtrip(t *testing.T) {
	gotCall := make(chan voice.Call, 1)
	srv := httptest.NewServer(Handler(func(ctx context.Context, c voice.Call) {
		gotCall <- c
		<-ctx.Done() // hold the call open until the test cancels
	}))
	defer srv.Close()

	url := strings.Replace(srv.URL, "http", "ws", 1) + "?rate=16000&id=call-1"
	cli, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer cli.Close()

	var call voice.Call
	select {
	case call = <-gotCall:
	case <-time.After(time.Second):
		t.Fatal("handler never received the call")
	}
	if call.ID() != "call-1" {
		t.Fatalf("id = %q", call.ID())
	}

	// Client → Call.In : send 2 PCM16 samples.
	pcm := make([]byte, 4)
	binary.LittleEndian.PutUint16(pcm[0:], uint16(int16(111)))
	binary.LittleEndian.PutUint16(pcm[2:], uint16(int16(222)))
	if err := cli.WriteMessage(websocket.BinaryMessage, pcm); err != nil {
		t.Fatalf("client write: %v", err)
	}
	select {
	case f := <-call.In():
		if f.Rate != 16000 || len(f.Samples) != 2 || f.Samples[0] != 111 || f.Samples[1] != 222 {
			t.Fatalf("inbound frame = %+v", f)
		}
	case <-time.After(time.Second):
		t.Fatal("Call.In never received the frame")
	}

	// Call.Out → client : send a frame, expect raw PCM bytes back.
	call.Out() <- voice.Frame{Samples: []int16{333, -333}, Rate: 16000}
	_ = cli.SetReadDeadline(timeNow().Add(time.Second))
	mt, data, err := cli.ReadMessage()
	if err != nil {
		t.Fatalf("client read: %v", err)
	}
	if mt != websocket.BinaryMessage || len(data) != 4 {
		t.Fatalf("outbound = type %d len %d", mt, len(data))
	}
	if int16(binary.LittleEndian.Uint16(data[0:])) != 333 || int16(binary.LittleEndian.Uint16(data[2:])) != -333 {
		t.Fatalf("outbound samples wrong: %v", data)
	}
}

// TestWSTransport_HangupClosesIn proves the client closing the socket closes Call.In.
func TestWSTransport_HangupClosesIn(t *testing.T) {
	gotCall := make(chan voice.Call, 1)
	srv := httptest.NewServer(Handler(func(ctx context.Context, c voice.Call) {
		gotCall <- c
		<-ctx.Done()
	}))
	defer srv.Close()

	url := strings.Replace(srv.URL, "http", "ws", 1)
	cli, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	call := <-gotCall
	_ = cli.Close()

	select {
	case _, ok := <-call.In():
		if ok {
			// drain any buffered frame, then expect close
			for ok {
				_, ok = <-call.In()
			}
		}
	case <-time.After(time.Second):
		t.Fatal("Call.In not closed after client hangup")
	}
}

var _ = http.MethodGet

// timeNow is a tiny indirection so the test reads naturally.
func timeNow() time.Time { return time.Now() }
