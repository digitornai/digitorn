// Package socketio is the Socket.IO transport adapter for the daemon.
//
// # Wiring
//
//	io := socketio.New(socketio.DefaultOptions(), logger)
//	io.SetAuthHandler(func(ctx context.Context, token string, _ map[string]any) error {
//	    return jwt.Validate(token)
//	})
//	io.OnConnection(func(ctx context.Context, c ports.RealtimeClient) error {
//	    // join a per-user room so server-side emits target this user
//	    return c.Join("user:" + extractUser(c))
//	})
//	io.OnEvent("chat", func(ctx context.Context, c ports.RealtimeClient, data any) error {
//	    return c.Emit("chat:ack", map[string]string{"status": "received"})
//	})
//
//	chiRouter.Handle("/socket.io/*", io.Handler())
//
// # Patterns for stability
//
//   - Every handler runs inside recover() — no panic propagates out of the package.
//   - Per-handler context timeouts (default 30s) prevent unbounded operations.
//   - Auth runs as middleware at handshake — invalid tokens never reach app code.
//   - Use namespaces ("/apps/<app_id>") to isolate per-app traffic.
//   - Use rooms ("session:<id>", "user:<id>") for targeted emits.
//   - Set MaxDisconnectionDuration > 0 to enable Connection State Recovery
//     (clients that briefly drop can resume without losing buffered events).
//
// # Scaling
//
// For horizontal scaling, wire the official Redis adapter:
//
//	github.com/zishang520/socket.io-go-redis
//
// Set Options.RedisURL and instantiate the adapter — the rest of the code is
// unchanged because rooms become cluster-wide automatically.
package socketio
