package socketio

import (
	"log/slog"
	"runtime/debug"

	socket "github.com/zishang520/socket.io/servers/socket/v3"

	"github.com/digitornai/digitorn/internal/ports"
)

type client struct {
	raw    *socket.Socket
	logger *slog.Logger
}

func newClient(raw *socket.Socket, logger *slog.Logger) *client {
	if logger == nil {
		logger = slog.Default()
	}
	return &client{raw: raw, logger: logger}
}

func (c *client) ID() string { return string(c.raw.Id()) }

func (c *client) Rooms() []string {
	set := c.raw.Rooms()
	if set == nil {
		return nil
	}
	keys := set.Keys()
	out := make([]string, len(keys))
	for i, r := range keys {
		out[i] = string(r)
	}
	return out
}

func (c *client) Join(room string) error {
	c.raw.Join(socket.Room(room))
	return nil
}

func (c *client) Leave(room string) error {
	c.raw.Leave(socket.Room(room))
	return nil
}

func (c *client) Emit(event string, data any) error {
	defer func() {
		if r := recover(); r != nil {
			c.logger.Error("socketio: client.Emit panic",
				slog.String("client_id", c.ID()),
				slog.String("event", event),
				slog.Any("panic", r),
				slog.String("stack", string(debug.Stack())),
			)
		}
	}()
	return c.raw.Emit(event, data)
}

func (c *client) Disconnect() { c.raw.Disconnect(true) }

func (c *client) Auth() map[string]any {
	hs := c.raw.Handshake()
	if hs == nil {
		return nil
	}
	return hs.Auth
}

var _ ports.RealtimeClient = (*client)(nil)
