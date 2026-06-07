package realtime

import (
	"context"
	"encoding/json"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// DialGateway opens a realtime Conn to the gateway's transparent /v1/realtime proxy
// (bifrost-http). The gateway resolves the provider + key from ITS config and proxies
// to the upstream model (OpenAI Realtime), so we only authenticate to the gateway and
// speak the provider's event protocol. Events are JSON TEXT frames both ways — the
// audio rides inside input_audio_buffer.append as base64, never as binary.
func DialGateway(ctx context.Context, baseURL, token, model string) (Conn, error) {
	h := http.Header{}
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 10 * time.Second}).DialContext(ctx, realtimeURL(baseURL, model), h)
	if err != nil {
		return nil, err
	}
	c := &gatewayConn{conn: conn, events: make(chan map[string]any, 256)}
	go c.read()
	return c, nil
}

// realtimeURL turns the gateway base URL into its /v1/realtime WS endpoint.
func realtimeURL(baseURL, model string) string {
	base := strings.Replace(strings.Replace(baseURL, "https://", "wss://", 1), "http://", "ws://", 1)
	base = strings.TrimRight(base, "/")
	u := base + "/v1/realtime"
	if model != "" {
		u += "?model=" + neturl.QueryEscape(model)
	}
	return u
}

type gatewayConn struct {
	conn      *websocket.Conn
	events    chan map[string]any
	wmu       sync.Mutex
	closeOnce sync.Once
}

// Send marshals one client event to a JSON text frame (the gateway only accepts text).
func (c *gatewayConn) Send(ev map[string]any) error {
	b, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	return c.conn.WriteMessage(websocket.TextMessage, b)
}

func (c *gatewayConn) Events() <-chan map[string]any { return c.events }

func (c *gatewayConn) Close() error {
	c.closeOnce.Do(func() { _ = c.conn.Close() })
	return nil
}

// read decodes provider events off the socket onto the Events channel until it closes.
func (c *gatewayConn) read() {
	defer close(c.events)
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		c.events <- m
	}
}

var _ Conn = (*gatewayConn)(nil)
