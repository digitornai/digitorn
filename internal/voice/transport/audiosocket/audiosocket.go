// Package audiosocket implements the Asterisk AudioSocket transport: a tiny TCP
// protocol where each connection is one call and frames are [1B type][2B len BE]
// [payload]. Types: 0x00 hangup, 0x01 UUID (16B call id), 0x10 audio (signed
// linear 16-bit, 8 kHz, mono). It's small enough to own in pure Go (no dep) — we
// reserve library-wrapping for the hard transports (WebRTC/SIP/Opus). It plugs
// into the voice.Transport seam, so the orchestrator never knows it's Asterisk.
package audiosocket

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net"
	"sync"

	"github.com/digitornai/digitorn/internal/voice"
)

const (
	kindHangup byte = 0x00
	kindID     byte = 0x01
	kindAudio  byte = 0x10

	sampleRate = 8000
)

// Transport is an AudioSocket TCP server. One inbound connection = one call.
type Transport struct {
	addr string
	mu   sync.Mutex
	ln   net.Listener
}

// New builds a transport bound to addr (e.g. ":9092").
func New(addr string) *Transport { return &Transport{addr: addr} }

func (t *Transport) Name() string { return "asterisk" }

// Addr returns the bound address (useful when listening on :0 in tests).
func (t *Transport) Addr() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.ln != nil {
		return t.ln.Addr().String()
	}
	return t.addr
}

// Serve accepts calls until ctx is cancelled, handing each to the orchestrator.
func (t *Transport) Serve(ctx context.Context, handler voice.CallHandler) error {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.ln = ln
	t.mu.Unlock()
	go func() { <-ctx.Done(); _ = ln.Close() }()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go t.serveConn(ctx, conn, handler)
	}
}

func (t *Transport) serveConn(ctx context.Context, conn net.Conn, handler voice.CallHandler) {
	defer conn.Close()
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()

	c := &call{
		conn:   conn,
		caller: conn.RemoteAddr().String(),
		in:     make(chan voice.Frame, 64),
		out:    make(chan voice.Frame, 256),
		cancel: cancel,
	}

	// Reader: parse inbound frames → call.In.
	go func() {
		defer cancel()
		r := bufio.NewReader(conn)
		for {
			kind, payload, err := readMsg(r)
			if err != nil {
				close(c.in)
				return
			}
			switch kind {
			case kindID:
				c.setID(hex.EncodeToString(payload))
			case kindAudio:
				select {
				case c.in <- decode(payload):
				case <-cctx.Done():
					return
				}
			case kindHangup:
				close(c.in)
				return
			}
		}
	}()

	// Writer: call.Out → outbound audio frames.
	go func() {
		for {
			select {
			case <-cctx.Done():
				return
			case f, ok := <-c.out:
				if !ok {
					return
				}
				if err := c.writeMsg(kindAudio, encode(f)); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	handler(cctx, c)
}

// call implements voice.Call over one AudioSocket connection.
type call struct {
	conn   net.Conn
	caller string
	in     chan voice.Frame
	out    chan voice.Frame
	cancel context.CancelFunc

	mu  sync.Mutex // guards id
	id  string
	wmu sync.Mutex // serializes writes to conn
}

func (c *call) setID(id string) {
	c.mu.Lock()
	c.id = id
	c.mu.Unlock()
}

func (c *call) ID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.id != "" {
		return c.id
	}
	return c.caller
}
func (c *call) Caller() string          { return c.caller }
func (c *call) In() <-chan voice.Frame  { return c.in }
func (c *call) Out() chan<- voice.Frame { return c.out }

func (c *call) Hangup() error {
	_ = c.writeMsg(kindHangup, nil)
	c.cancel()
	return nil
}

func (c *call) writeMsg(kind byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [3]byte
	hdr[0] = kind
	binary.BigEndian.PutUint16(hdr[1:], uint16(len(payload)))
	if _, err := c.conn.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		_, err := c.conn.Write(payload)
		return err
	}
	return nil
}

// ── framing + codec ──────────────────────────────────────────────────────────

func readMsg(r *bufio.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	n := binary.BigEndian.Uint16(hdr[1:])
	if n == 0 {
		return hdr[0], nil, nil
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(r, payload); err != nil {
		return 0, nil, err
	}
	return hdr[0], payload, nil
}

// decode turns little-endian signed-linear PCM16 bytes into a Frame.
func decode(b []byte) voice.Frame {
	n := len(b) / 2
	s := make([]int16, n)
	for i := 0; i < n; i++ {
		s[i] = int16(binary.LittleEndian.Uint16(b[2*i:]))
	}
	return voice.Frame{Samples: s, Rate: sampleRate}
}

// encode turns a Frame into little-endian PCM16 bytes for Asterisk.
func encode(f voice.Frame) []byte {
	b := make([]byte, len(f.Samples)*2)
	for i, x := range f.Samples {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(x))
	}
	return b
}
