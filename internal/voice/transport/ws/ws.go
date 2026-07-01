// Package ws is the daemon-side audio transport: a WebSocket endpoint the voice
// adapter (digitorn-voice) connects to and streams the call's audio over. Inbound
// binary frames are raw PCM16 mono at the connection's negotiated rate; the engine's
// outbound audio is encoded back the same way. No base64, no JSON — the latency path.
//
// It is mounted on the daemon's HTTP server (auth-gated there); each accepted
// connection becomes one voice.Call handed to the orchestrator.
package ws

import (
	"context"
	"encoding/binary"
	"net/http"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/digitornai/digitorn/internal/voice"
)

const defaultRate = 8000

// Handler upgrades incoming WebSocket connections and runs onCall for each (blocking
// until the call ends). The PCM rate is read from ?rate= (default 8000); ?id / ?caller
// label the call. Auth is enforced by the server mount, so CheckOrigin is permissive.
func Handler(onCall func(ctx context.Context, c voice.Call)) http.Handler {
	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true },
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		q := r.URL.Query()
		rate := defaultRate
		if v, err := strconv.Atoi(q.Get("rate")); err == nil && v > 0 {
			rate = v
		}
		ctx, cancel := context.WithCancel(r.Context())
		c := &call{
			conn:   conn,
			id:     q.Get("id"),
			caller: q.Get("caller"),
			rate:   rate,
			in:     make(chan voice.Frame, 64),
			out:    make(chan voice.Frame, 256),
			cancel: cancel,
		}
		if c.id == "" {
			c.id = conn.RemoteAddr().String()
		}
		go c.readLoop(ctx)
		go c.writeLoop(ctx)
		onCall(ctx, c)
		_ = c.Close()
	})
}

// call implements voice.Call over one WebSocket connection.
type call struct {
	conn   *websocket.Conn
	id     string
	caller string
	rate   int
	in     chan voice.Frame
	out    chan voice.Frame
	cancel context.CancelFunc

	wmu       sync.Mutex
	closeOnce sync.Once
	inOnce    sync.Once
}

func (c *call) ID() string              { return c.id }
func (c *call) Caller() string          { return c.caller }
func (c *call) In() <-chan voice.Frame  { return c.in }
func (c *call) Out() chan<- voice.Frame { return c.out }

func (c *call) Hangup() error {
	c.cancel()
	return c.Close()
}

func (c *call) Close() error {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.conn.Close()
	})
	return nil
}

// readLoop pumps inbound WS binary frames → decoded PCM frames on In, closing In once
// when the socket ends.
func (c *call) readLoop(ctx context.Context) {
	defer c.inOnce.Do(func() { close(c.in) })
	defer c.cancel()
	for {
		mt, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(data) < 2 {
			continue
		}
		select {
		case c.in <- voice.Frame{Samples: decodePCM16(data), Rate: c.rate}:
		case <-ctx.Done():
			return
		}
	}
}

// writeLoop encodes outbound PCM frames → WS binary messages until Out closes or the
// call ends.
func (c *call) writeLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case f, ok := <-c.out:
			if !ok {
				return
			}
			if err := c.write(encodePCM16(f.Samples)); err != nil {
				c.cancel()
				return
			}
		}
	}
}

func (c *call) write(b []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.WriteMessage(websocket.BinaryMessage, b)
}

func decodePCM16(b []byte) []int16 {
	n := len(b) / 2
	s := make([]int16, n)
	for i := range n {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return s
}

func encodePCM16(s []int16) []byte {
	b := make([]byte, len(s)*2)
	for i, v := range s {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}

var _ voice.Call = (*call)(nil)
