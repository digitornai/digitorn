package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
)

// Manager owns a pool of supervised workers and exposes them through
// load-balanced gRPC connections. Modules call Pool(kind) to get one or
// more healthy handles, then dial via the embedded credentials.
type Manager struct {
	logger *slog.Logger

	mu      sync.RWMutex
	pools   map[Kind]*pool
	stopped atomic.Bool
	started atomic.Bool
}

type pool struct {
	spec    Spec
	supers  []*supervisor
	clients []*managedConn
	rr      atomic.Uint64
}

func NewManager(logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	return &Manager{logger: logger, pools: map[Kind]*pool{}}
}

// Start marks the manager as started. Spawn() is safe before or after Start.
func (m *Manager) Start() error {
	m.started.Store(true)
	return nil
}

// Spawn registers a worker spec and starts Count instances. If the kind
// is already registered, ErrInvalidSpec is returned.
func (m *Manager) Spawn(ctx context.Context, spec Spec) error {
	if spec.Kind == "" || spec.Binary == "" {
		return fmt.Errorf("%w: kind and binary required", ErrInvalidSpec)
	}
	spec.applyDefaults()

	m.mu.Lock()
	if _, ok := m.pools[spec.Kind]; ok {
		m.mu.Unlock()
		return fmt.Errorf("%w: kind %q already registered", ErrInvalidSpec, spec.Kind)
	}
	m.mu.Unlock()

	// Build the pool's slices in locals and start every instance before the
	// pool is ever published into m.pools. Readers (Pool/Pick/Stats) only
	// touch p.supers/p.clients after fetching p under the lock, so a pool
	// reachable from the map is always fully constructed and immutable.
	supers := make([]*supervisor, 0, spec.Count)
	clients := make([]*managedConn, 0, spec.Count)
	var startErrs []error
	for i := 0; i < spec.Count; i++ {
		id := fmt.Sprintf("%s#%d", spec.Kind, i)
		sup := newSupervisor(id, spec, m.logger)
		mc := &managedConn{sup: sup, secret: sup.envSec}
		supers = append(supers, sup)
		clients = append(clients, mc)
		if err := sup.Start(ctx); err != nil {
			startErrs = append(startErrs, fmt.Errorf("%s: %w", id, err))
			continue
		}
		m.logger.Info("worker: instance ready",
			slog.String("id", id),
			slog.String("kind", string(spec.Kind)),
			slog.String("addr", *sup.address.Load()))
	}

	p := &pool{spec: spec, supers: supers, clients: clients}
	m.mu.Lock()
	if _, ok := m.pools[spec.Kind]; ok {
		m.mu.Unlock()
		for i, s := range supers {
			_ = clients[i].Close()
			_ = s.Stop(context.Background())
		}
		return fmt.Errorf("%w: kind %q already registered", ErrInvalidSpec, spec.Kind)
	}
	m.pools[spec.Kind] = p
	m.mu.Unlock()

	if len(startErrs) > 0 {
		return errors.Join(startErrs...)
	}
	return nil
}

// Pool returns the live handles for a given kind, in arbitrary order.
func (m *Manager) Pool(kind Kind) []Handle {
	m.mu.RLock()
	p, ok := m.pools[kind]
	m.mu.RUnlock()
	if !ok {
		return nil
	}
	out := make([]Handle, 0, len(p.supers))
	for _, s := range p.supers {
		out = append(out, s.snapshot())
	}
	return out
}

// Pick selects the next healthy worker for `kind` via round-robin. Returns
// ErrNoHealthyWorker if none are SERVING. The returned Conn is owned by
// the framework — the caller must NOT Close it.
//
// Phase-3 change: Pick now returns a `pickedConn` wrapper that pins ONE
// specific gRPC ClientConn from the worker's per-worker conn pool (size
// `connPoolSize`, default 8). This lets HTTP/2 stream multiplexing
// spread across multiple TCP/HTTP/2 conns instead of saturating the
// per-conn `MaxConcurrentStreams` (default ~100). At 10K concurrent
// RPCs to one worker, 1 conn = 100-stream cap = stalls; 8 conns = 800-stream
// cap = 8x headroom — and any over-budget call backs off gracefully via
// HTTP/2 flow control instead of head-of-line blocking.
func (m *Manager) Pick(ctx context.Context, kind Kind) (Conn, error) {
	if m.stopped.Load() {
		return nil, ErrManagerStopped
	}
	m.mu.RLock()
	p, ok := m.pools[kind]
	m.mu.RUnlock()
	if !ok {
		return nil, ErrNoHealthyWorker
	}
	for tries := 0; tries < len(p.clients)*2; tries++ {
		idx := int(p.rr.Add(1)-1) % len(p.clients)
		mc := p.clients[idx]
		if mc.sup.status.Load() == int32(StatusReady) || mc.sup.status.Load() == int32(StatusRunning) {
			conn, err := mc.ensureConn(ctx)
			if err != nil {
				continue
			}
			// Don't trust the supervisor's local status alone: a backend can
			// be marked Ready yet have a wedged or dead channel. Skip conns
			// the gRPC layer already knows are failing.
			conn.Connect()
			if st := conn.GetState(); st == connectivity.TransientFailure || st == connectivity.Shutdown {
				continue
			}
			// Pin the specific conn we tested above into a lightweight
			// wrapper so the caller's mc.GRPC() returns exactly THIS
			// conn — without it, a concurrent Pick on the same worker
			// would round-robin the caller's GRPC() into a different
			// (potentially untested) slot.
			return &pickedConn{mc: mc, conn: conn}, nil
		}
	}
	return nil, ErrNoHealthyWorker
}

// Stop drains and stops every supervised worker, in parallel, with a
// per-instance StopTimeout.
func (m *Manager) Stop(ctx context.Context) error {
	if !m.stopped.CompareAndSwap(false, true) {
		return nil
	}
	m.mu.Lock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.Unlock()

	var wg sync.WaitGroup
	var stopMu sync.Mutex
	var stopErrs []error
	for _, p := range pools {
		for i, s := range p.supers {
			wg.Add(1)
			s := s
			mc := p.clients[i]
			go func() {
				defer wg.Done()
				_ = mc.Close()
				// Drain on a fresh context: the caller's ctx is often already
				// cancelled during shutdown, which would skip the graceful stop.
				stopCtx, cancel := context.WithTimeout(context.Background(), p.spec.StopTimeout+1*time.Second)
				defer cancel()
				if err := s.Stop(stopCtx); err != nil {
					stopMu.Lock()
					stopErrs = append(stopErrs, err)
					stopMu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(stopErrs) > 0 {
		return errors.Join(stopErrs...)
	}
	return nil
}

// Stats is a snapshot for /diagnostics and Prometheus.
type Stats struct {
	Pools map[Kind]PoolStats
}
type PoolStats struct {
	Total    int
	Ready    int
	Failed   int
	Restarts int
}

func (m *Manager) Stats() Stats {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := Stats{Pools: make(map[Kind]PoolStats, len(m.pools))}
	for k, p := range m.pools {
		ps := PoolStats{Total: len(p.supers)}
		for _, s := range p.supers {
			st := Status(s.status.Load())
			switch st {
			case StatusReady, StatusRunning:
				ps.Ready++
			case StatusFailed:
				ps.Failed++
			}
			ps.Restarts += int(s.restarts.Load())
		}
		out.Pools[k] = ps
	}
	return out
}

// ------- gRPC client wiring -------

// connPoolSize is the number of parallel HTTP/2 connections kept open
// per worker. HTTP/2 multiplexes streams over one TCP conn but caps at
// MaxConcurrentStreams (server default ~100). For our 1M-agent target a
// single conn would head-of-line block hard. 8 conns = 800-stream
// budget per worker, which keeps the dispatch loop scalable while
// remaining well under socket-table limits.
//
// Round-robin across the slots is one atomic.Add per Pick (cheap, no
// locking). Per-slot dial is gated by a per-slot mutex so two callers
// can dial DIFFERENT slots in parallel — a hot worker doesn't bottleneck
// on a single mutex during the first-N requests.
const connPoolSize = 8

// connSlot is one entry in managedConn's connection pool. The fast
// read path is one atomic.Pointer.Load (~5 ns). The slow dial path
// takes the slot mutex but only once per (worker, slot) lifetime
// (or when the worker re-binds a new port).
type connSlot struct {
	mu       sync.Mutex
	conn     atomic.Pointer[grpc.ClientConn]
	connAddr atomic.Pointer[string] // address the cached conn was dialed to
}

// managedConn lazily dials a POOL of N gRPC connections per worker and
// caches them. It also injects the shared secret header on every
// outgoing RPC. If the worker restarts and binds a new port, every
// cached slot is closed lazily and a fresh conn dialed against the new
// address.
//
// Phase-3 change: replaced the single conn + global mu with N atomic-
// pointer slots. The hot-path read is lock-free; the dial path takes
// only the affected slot's mutex.
type managedConn struct {
	sup    *supervisor
	secret string

	// slotRR is the per-managedConn round-robin index across slots.
	// One atomic.Add per ensureConn call (~1 ns).
	slotRR atomic.Uint64

	// slots holds the N connection slots. Index = slotRR.Add(1) % N.
	slots [connPoolSize]connSlot
}

// ensureConn picks the next round-robin slot, ensures it has a live
// conn pointing at the worker's current address, and returns it. Hot
// path is one atomic.Add + one atomic.Load when the conn is warm. Cold
// path (first request, or worker re-bound a new port) dials under the
// slot mutex only.
func (m *managedConn) ensureConn(ctx context.Context) (*grpc.ClientConn, error) {
	addrPtr := m.sup.address.Load()
	if addrPtr == nil || *addrPtr == "" {
		return nil, ErrNoHealthyWorker
	}
	target := *addrPtr
	idx := int(m.slotRR.Add(1)-1) % connPoolSize
	return m.ensureSlot(idx, target)
}

// ensureSlot is the per-slot logic. Fast path: if the slot has a live
// conn pointing at `target`, return it. Slow path: take the slot's
// mutex and dial. Re-checked under the lock so two callers don't
// double-dial.
func (m *managedConn) ensureSlot(idx int, target string) (*grpc.ClientConn, error) {
	slot := &m.slots[idx]
	// Fast path — lock-free.
	if p := slot.conn.Load(); p != nil {
		if a := slot.connAddr.Load(); a != nil && *a == target {
			if p.GetState().String() != "SHUTDOWN" {
				return p, nil
			}
		}
	}
	// Slow path — dial under the slot mutex.
	slot.mu.Lock()
	defer slot.mu.Unlock()
	// Re-check under the lock to avoid duplicate dials.
	if p := slot.conn.Load(); p != nil {
		if a := slot.connAddr.Load(); a != nil && *a == target && p.GetState().String() != "SHUTDOWN" {
			return p, nil
		}
		// Stale or pointing at a different address: tear it down.
		_ = p.Close()
		slot.conn.Store(nil)
		slot.connAddr.Store(nil)
	}
	conn, err := m.dial(target)
	if err != nil {
		return nil, err
	}
	slot.conn.Store(conn)
	tgt := target
	slot.connAddr.Store(&tgt)
	return conn, nil
}

// dial constructs a fresh grpc.ClientConn with the standard dial
// options. Used by ensureSlot on the cold path. Extracted into its own
// function so the dial code (which knows about unix sockets etc.) lives
// in one place.
func (m *managedConn) dial(target string) (*grpc.ClientConn, error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithUnaryInterceptor(m.unaryAuthInterceptor()),
		grpc.WithStreamInterceptor(m.streamAuthInterceptor()),
	}
	// A "unix:<path>" address dials an AF_UNIX socket via an explicit
	// dialer ; a bare host:port dials TCP through the default resolver.
	dialTarget := target
	if path, ok := strings.CutPrefix(target, "unix:"); ok {
		dialTarget = "passthrough:///" + path
		opts = append(opts, grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", path)
		}))
	}
	conn, err := grpc.NewClient(dialTarget, opts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", target, err)
	}
	return conn, nil
}

func (m *managedConn) unaryAuthInterceptor() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		ctx = metadata.AppendToOutgoingContext(ctx,
			HeaderSecret, m.secret,
			HeaderWorkerKind, string(m.sup.spec.Kind))
		return invoker(ctx, method, req, reply, cc, opts...)
	}
}

func (m *managedConn) streamAuthInterceptor() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, cc *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		ctx = metadata.AppendToOutgoingContext(ctx,
			HeaderSecret, m.secret,
			HeaderWorkerKind, string(m.sup.spec.Kind))
		return streamer(ctx, desc, cc, method, opts...)
	}
}

// GRPC returns one round-robin conn from the per-worker pool. Kept on
// managedConn for back-compat — code paths that pin a specific conn
// via Pick() now wrap into pickedConn instead. Calling this directly is
// equivalent to ensureConn() + ignoring errors (returns nil when no
// conn can be dialed) — kept for the Conn interface.
//
// Returns nil if no slot has a usable conn AND the worker has no
// resolvable address. Callers should treat nil as a transport error.
func (m *managedConn) GRPC() *grpc.ClientConn {
	addrPtr := m.sup.address.Load()
	if addrPtr == nil || *addrPtr == "" {
		// No address: return the first conn we find (best effort).
		for i := range m.slots {
			if p := m.slots[i].conn.Load(); p != nil {
				return p
			}
		}
		return nil
	}
	idx := int(m.slotRR.Add(1)-1) % connPoolSize
	conn, _ := m.ensureSlot(idx, *addrPtr)
	return conn
}

func (m *managedConn) Handle() Handle { return m.sup.snapshot() }

// Close drains every slot in the pool. Idempotent: returns the first
// non-nil close error. Called from Manager.Stop during graceful
// shutdown. Slot mutexes serialise against concurrent dial-on-demand
// to avoid double-close races.
func (m *managedConn) Close() error {
	var first error
	for i := range m.slots {
		slot := &m.slots[i]
		slot.mu.Lock()
		if p := slot.conn.Load(); p != nil {
			if err := p.Close(); err != nil && first == nil {
				first = err
			}
			slot.conn.Store(nil)
			slot.connAddr.Store(nil)
		}
		slot.mu.Unlock()
	}
	return first
}

// pickedConn is the Conn wrapper returned by Manager.Pick. It pins one
// specific *grpc.ClientConn (the one Pick state-checked) so the
// caller's GRPC() returns exactly THAT conn, instead of round-robining
// into a different (potentially untested) slot. The wrapper does NOT
// own the conn — Close is a no-op because the framework manages slot
// lifecycle via managedConn.Close.
type pickedConn struct {
	mc   *managedConn
	conn *grpc.ClientConn
}

func (p *pickedConn) GRPC() *grpc.ClientConn { return p.conn }
func (p *pickedConn) Handle() Handle         { return p.mc.sup.snapshot() }
func (p *pickedConn) Close() error           { return nil }

// HealthCheck performs a synchronous gRPC health check against the worker.
func HealthCheck(ctx context.Context, c Conn, service string) (grpc_health_v1.HealthCheckResponse_ServingStatus, error) {
	cli := grpc_health_v1.NewHealthClient(c.GRPC())
	resp, err := cli.Check(ctx, &grpc_health_v1.HealthCheckRequest{Service: service})
	if err != nil {
		return grpc_health_v1.HealthCheckResponse_NOT_SERVING, err
	}
	return resp.Status, nil
}
