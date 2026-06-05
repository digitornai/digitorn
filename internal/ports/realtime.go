package ports

import "context"

// RealtimeMessage is an event sent to clients over the realtime channel.
type RealtimeMessage struct {
	Namespace string `json:"namespace,omitempty"`
	Room      string `json:"room,omitempty"`
	Event     string `json:"event"`
	Data      any    `json:"data"`
}

// RealtimeClient identifies a connected client.
type RealtimeClient interface {
	ID() string
	Rooms() []string
	Join(room string) error
	Leave(room string) error
	Emit(event string, data any) error
	Disconnect()
	// Auth returns the client's handshake auth map (the same map the
	// AuthHandler received). Mutations to this map by the AuthHandler
	// persist and are readable here — that's how the bridge propagates
	// the validated user_id from auth-time to connection-time.
	Auth() map[string]any
}

// ConnectionHandler is invoked for every new realtime connection.
type ConnectionHandler func(ctx context.Context, client RealtimeClient) error

// DisconnectHandler is invoked when a client disconnects, with its id, so
// per-connection state can be released.
type DisconnectHandler func(clientID string)

// EventHandler handles events received from a client.
type EventHandler func(ctx context.Context, client RealtimeClient, data any) error

// AuthHandler validates a connection at handshake time. Returning an error
// rejects the connection.
type AuthHandler func(ctx context.Context, token string, metadata map[string]any) error

// RealtimeServer abstracts the realtime transport (Socket.IO, Centrifuge, raw WS).
// Encapsulating the implementation behind this interface lets us swap the
// underlying library (currently zishang520/socket.io) without touching the
// rest of the codebase.
type RealtimeServer interface {
	// SetAuthHandler installs a handshake-time auth check.
	SetAuthHandler(h AuthHandler)

	// OnConnection registers a handler invoked for each new client.
	OnConnection(h ConnectionHandler)

	// OnDisconnection registers a handler invoked when a client disconnects,
	// so per-connection state can be freed.
	OnDisconnection(h DisconnectHandler)

	// OnEvent registers a handler for a named event.
	OnEvent(event string, h EventHandler)

	// Emit sends an event to a specific room (or "" for global broadcast).
	Emit(ctx context.Context, namespace, room, event string, data any) error

	// Broadcast sends an event to every client in a namespace.
	Broadcast(ctx context.Context, namespace, event string, data any) error

	// Close gracefully shuts down the server, disconnecting all clients.
	Close(ctx context.Context) error
}
