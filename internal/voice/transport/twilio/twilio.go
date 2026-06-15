// Package twilio implements the Twilio Media Streams transport : Twilio dials
// OUR WebSocket (the <Stream url="wss://…"/> TwiML verb) and exchanges JSON
// events whose audio payload is base64 G.711 μ-law at 8 kHz mono. This transport
// owns that wire codec — inbound payloads are decoded to PCM16 Frames and the
// engine's outbound audio is companded back — so the orchestrator never knows
// the call came from Twilio. Bidirectional streams are assumed (outbound media
// + clear are only honoured by Twilio on <Connect><Stream>).
//
// Wire protocol (Twilio "Media Streams" v1):
//
//	in  : {"event":"connected"} → {"event":"start","start":{streamSid,callSid,
//	      mediaFormat{encoding:"audio/x-mulaw",sampleRate:8000}}} →
//	      {"event":"media","media":{"payload":"<b64 μ-law>"}}* → {"event":"stop"}
//	out : {"event":"media","streamSid":…,"media":{"payload":"<b64 μ-law>"}}
//	      {"event":"clear","streamSid":…}   (flush Twilio's playback buffer)
package twilio

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/mbathepaul/digitorn/internal/voice"
	"github.com/mbathepaul/digitorn/internal/voice/codec"
)

const sampleRate = 8000 // Twilio Media Streams is fixed at 8 kHz μ-law mono

// Transport is a Twilio Media Streams WebSocket server. One stream = one call.
type Transport struct {
	addr string
	path string

	mu sync.Mutex
	ln net.Listener
}

// New builds a transport bound to addr (e.g. ":9093"). path is the WS route
// Twilio is pointed at ("" = accept any path, the usual case behind a tunnel).
func New(addr, path string) *Transport { return &Transport{addr: addr, path: path} }

func (t *Transport) Name() string { return "twilio" }

// Addr returns the bound address (useful when listening on :0 in tests).
func (t *Transport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln != nil {
		return t.ln.Addr().String()
	}
	return t.addr
}

// Serve accepts Twilio stream connections until ctx ends, handing each call to
// the orchestrator handler once its "start" event arrives.
func (t *Transport) Serve(ctx context.Context, handler voice.CallHandler) error {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.ln = ln
	t.mu.Unlock()

	up := websocket.Upgrader{
		ReadBufferSize:  4096,
		WriteBufferSize: 4096,
		CheckOrigin:     func(*http.Request) bool { return true }, // Twilio sends no browser Origin
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t.path != "" && r.URL.Path != t.path {
			http.NotFound(w, r)
			return
		}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		t.serveStream(ctx, conn, handler)
	})}
	go func() { <-ctx.Done(); _ = srv.Close() }()
	if err := srv.Serve(ln); err != nil && ctx.Err() == nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// inboundEvent is the subset of Twilio's stream events we consume.
type inboundEvent struct {
	Event string `json:"event"`
	Start *struct {
		StreamSid string `json:"streamSid"`
		CallSid   string `json:"callSid"`
	} `json:"start"`
	Media *struct {
		Track   string `json:"track"`
		Payload string `json:"payload"`
	} `json:"media"`
	StreamSid string `json:"streamSid"`
}

// serveStream drives one Twilio stream : waits for "start" (which carries the
// ids), then runs the call handler with the decoding/encoding pumps attached.
func (t *Transport) serveStream(ctx context.Context, conn *websocket.Conn, handler voice.CallHandler) {
	defer conn.Close()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &call{
		conn:   conn,
		in:     make(chan voice.Frame, 64),
		cancel: cancel,
	}

	// Phase 1 : consume events until "start" — only then do we know the
	// streamSid required to address outbound media.
	for c.sid == "" {
		var ev inboundEvent
		if err := conn.ReadJSON(&ev); err != nil {
			return
		}
		switch ev.Event {
		case "start":
			if ev.Start != nil {
				c.sid = ev.Start.StreamSid
				c.callSid = ev.Start.CallSid
			}
			if c.sid == "" {
				c.sid = ev.StreamSid
			}
		case "stop":
			return
		}
	}

	go c.readLoop(cctx)
	handler(cctx, c)
	_ = c.Close()
}

// call implements voice.Call over one Twilio media stream.
type call struct {
	conn    *websocket.Conn
	sid     string // streamSid — addresses outbound media/clear
	callSid string
	in      chan voice.Frame
	cancel  context.CancelFunc

	wmu       sync.Mutex
	out       chan voice.Frame
	outOnce   sync.Once
	inOnce    sync.Once
	closeOnce sync.Once
}

func (c *call) ID() string {
	if c.callSid != "" {
		return c.callSid
	}
	return c.sid
}
func (c *call) Caller() string         { return c.callSid }
func (c *call) In() <-chan voice.Frame { return c.in }

// Out lazily starts the encoding pump : PCM16 frames → μ-law → base64 →
// {"event":"media"} addressed to this stream.
func (c *call) Out() chan<- voice.Frame {
	c.outOnce.Do(func() {
		c.out = make(chan voice.Frame, 256)
		go func() {
			for f := range c.out {
				payload := base64.StdEncoding.EncodeToString(codec.MuLawEncodeAll(f.Samples))
				if err := c.writeJSON(map[string]any{
					"event":     "media",
					"streamSid": c.sid,
					"media":     map[string]any{"payload": payload},
				}); err != nil {
					c.cancel()
					return
				}
			}
		}()
	})
	return c.out
}

// ClearPlayback flushes Twilio's buffered outbound audio — the orchestrator
// calls it on barge-in so the caller stops hearing the stale reply instantly.
func (c *call) ClearPlayback() {
	_ = c.writeJSON(map[string]any{"event": "clear", "streamSid": c.sid})
}

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

// readLoop decodes inbound media events (base64 μ-law → PCM16 Frames) until the
// stream stops, closing In exactly once.
func (c *call) readLoop(ctx context.Context) {
	defer c.inOnce.Do(func() { close(c.in) })
	defer c.cancel()
	for {
		var ev inboundEvent
		if err := c.conn.ReadJSON(&ev); err != nil {
			return
		}
		switch ev.Event {
		case "media":
			if ev.Media == nil || ev.Media.Payload == "" {
				continue
			}
			// Bidirectional streams echo our outbound audio on track
			// "outbound" — only the caller's track feeds the engine.
			if ev.Media.Track == "outbound" {
				continue
			}
			raw, err := base64.StdEncoding.DecodeString(ev.Media.Payload)
			if err != nil || len(raw) == 0 {
				continue
			}
			select {
			case c.in <- voice.Frame{Samples: codec.MuLawDecodeAll(raw), Rate: sampleRate}:
			case <-ctx.Done():
				return
			}
		case "stop":
			return
		}
	}
}

func (c *call) writeJSON(v any) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

var (
	_ voice.Call            = (*call)(nil)
	_ voice.PlaybackClearer = (*call)(nil)
	_ voice.Transport       = (*Transport)(nil)
)
