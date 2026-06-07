package realtime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestDialGateway_Roundtrip proves the Conn speaks the gateway's realtime WS as JSON
// text both ways: a client event reaches the server, and a server event surfaces on
// Events(). It also verifies the /v1/realtime?model= path + bearer auth header.
func TestDialGateway_Roundtrip(t *testing.T) {
	var gotPath, gotModel, gotAuth string
	gotEvent := make(chan map[string]any, 1)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotModel = r.URL.Query().Get("model")
		gotAuth = r.Header.Get("Authorization")
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		// Echo one server→client event, then read one client→server event.
		_ = conn.WriteJSON(map[string]any{"type": "session.created"})
		var m map[string]any
		if conn.ReadJSON(&m) == nil {
			gotEvent <- m
		}
		<-time.After(50 * time.Millisecond)
	}))
	defer srv.Close()

	c, err := DialGateway(context.Background(), srv.URL, "tok", "gpt-realtime")
	if err != nil {
		t.Fatalf("DialGateway: %v", err)
	}
	defer c.Close()

	// Server → client event arrives on Events().
	select {
	case ev := <-c.Events():
		if ev["type"] != "session.created" {
			t.Fatalf("event = %v", ev["type"])
		}
	case <-time.After(time.Second):
		t.Fatal("no inbound event")
	}

	// Client → server event is delivered as JSON text.
	if err := c.Send(map[string]any{"type": "response.create"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case ev := <-gotEvent:
		if ev["type"] != "response.create" {
			t.Fatalf("server got %v", ev["type"])
		}
	case <-time.After(time.Second):
		t.Fatal("server did not receive the client event")
	}

	if gotPath != "/v1/realtime" || gotModel != "gpt-realtime" || !strings.Contains(gotAuth, "Bearer tok") {
		t.Fatalf("request: path=%q model=%q auth=%q", gotPath, gotModel, gotAuth)
	}
}

// TestRealtimeURL covers the scheme + path construction.
func TestRealtimeURL(t *testing.T) {
	cases := map[string]string{
		"http://127.0.0.1:8002":   "ws://127.0.0.1:8002/v1/realtime?model=m",
		"https://gw.example.com/":  "wss://gw.example.com/v1/realtime?model=m",
	}
	for in, want := range cases {
		if got := realtimeURL(in, "m"); got != want {
			t.Fatalf("realtimeURL(%q) = %q, want %q", in, got, want)
		}
	}
}
