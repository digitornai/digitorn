package socketio

import (
	"log/slog"
	"runtime/debug"

	socket "github.com/zishang520/socket.io/servers/socket/v3"

	"github.com/mbathepaul/digitorn/internal/ports"
)

// client wraps a *socket.Socket and implements ports.RealtimeClient.
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

// ID returns the unique Socket.IO ID of this client.
func (c *client) ID() string { return string(c.raw.Id()) }

// Rooms returns all rooms the client has joined.
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

// Join adds the client to a room.
func (c *client) Join(room string) error {
	c.raw.Join(socket.Room(room))
	return nil
}

// Leave removes the client from a room.
func (c *client) Leave(room string) error {
	c.raw.Leave(socket.Room(room))
	return nil
}

// Emit sends an event directly to this client.
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

// Disconnect forcibly disconnects the client.
func (c *client) Disconnect() { c.raw.Disconnect(true) }

// Auth returns the client's handshake auth map. The AuthHandler is
// expected to mutate this map (e.g. inject a validated `user_id`) so
// connection-time handlers can read it back here.
func (c *client) Auth() map[string]any {
	hs := c.raw.Handshake()
	if hs == nil {
		return nil
	}
	return hs.Auth
}

var _ ports.RealtimeClient = (*client)(nil)
