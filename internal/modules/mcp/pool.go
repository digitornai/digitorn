package mcp

import (
	"context"
	"math/rand"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

type mcpConn interface {
	listTools(context.Context) ([]*mcpsdk.Tool, error)
	callTool(context.Context, string, any) (*mcpsdk.CallToolResult, error)
	listResources(context.Context) ([]*mcpsdk.Resource, error)
	listPrompts(context.Context) ([]*mcpsdk.Prompt, error)
	getPrompt(context.Context, string, map[string]string) (*mcpsdk.GetPromptResult, error)
	readResource(context.Context, string) (*mcpsdk.ReadResourceResult, error)
	ping(context.Context) error
	close() error
}

type liveServer struct {
	id           string
	tools        []*mcpsdk.Tool
	hasPrompts   bool
	hasResources bool
}

type serverStatus string

const (
	statusConnected    serverStatus = "connected"
	statusError        serverStatus = "error"
	statusDisconnected serverStatus = "disconnected"
)

const (
	capTimeout  = 15 * time.Second
	pingTimeout = 10 * time.Second
)

type serverEntry struct {
	id        string
	spec      connectSpec
	conn      mcpConn
	tools     []*mcpsdk.Tool
	resources []*mcpsdk.Resource
	prompts   []*mcpsdk.Prompt
	status    serverStatus
	errMsg    string
	createdAt time.Time
	lastPing  time.Time
	failures  int
}

type serverSnapshot struct {
	ID        string
	Status    serverStatus
	Tools     int
	Resources int
	Prompts   int
	Err       string
}

func (e *serverEntry) snapshot() serverSnapshot {
	return serverSnapshot{
		ID: e.id, Status: e.status, Err: e.errMsg,
		Tools: len(e.tools), Resources: len(e.resources), Prompts: len(e.prompts),
	}
}

type pool struct {
	connMu     sync.Mutex
	mu         sync.RWMutex
	entries    map[string]*serverEntry
	maxRetries int
	base       time.Duration
	maxWait    time.Duration
	dialFn     func(context.Context, connectSpec) (mcpConn, error)
	now        func() time.Time
	rngMu      sync.Mutex
	rng        *rand.Rand
}

func newPool(maxRetries int) *pool {
	return &pool{
		entries:    map[string]*serverEntry{},
		maxRetries: maxRetries,
		base:       time.Second,
		maxWait:    30 * time.Second,
		dialFn:     defaultDial,
		now:        time.Now,
		rng:        rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func defaultDial(ctx context.Context, spec connectSpec) (mcpConn, error) {
	c, err := dial(ctx, spec)
	if err != nil {
		return nil, err
	}
	return c, nil
}

func (p *pool) getEntry(id string) *serverEntry {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.entries[id]
}

func (p *pool) putEntry(ent *serverEntry) {
	p.mu.Lock()
	p.entries[ent.id] = ent
	p.mu.Unlock()
}

func (p *pool) connect(ctx context.Context, id string, spec connectSpec) (serverSnapshot, error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()

	if old := p.getEntry(id); old != nil && old.conn != nil {
		_ = old.conn.close()
	}

	c, err := p.dialFn(ctx, spec)
	if err != nil {
		ent := &serverEntry{id: id, spec: spec, status: statusError, errMsg: err.Error(), createdAt: p.now()}
		p.putEntry(ent)
		return ent.snapshot(), classify(err)
	}
	ent := &serverEntry{id: id, spec: spec, conn: c, status: statusConnected, createdAt: p.now(), lastPing: p.now()}
	p.refreshCaps(ctx, ent)
	p.putEntry(ent)
	return ent.snapshot(), nil
}

func (p *pool) refreshCaps(ctx context.Context, ent *serverEntry) {
	tctx, cancel := context.WithTimeout(ctx, capTimeout)
	defer cancel()
	if tools, err := ent.conn.listTools(tctx); err == nil {
		ent.tools = tools
	}
	if res, err := ent.conn.listResources(tctx); err == nil {
		ent.resources = res
	}
	if pr, err := ent.conn.listPrompts(tctx); err == nil {
		ent.prompts = pr
	}
}

func (p *pool) disconnect(_ context.Context, id string) serverSnapshot {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	ent := p.getEntry(id)
	if ent != nil && ent.conn != nil {
		_ = ent.conn.close()
	}
	p.mu.Lock()
	delete(p.entries, id)
	p.mu.Unlock()
	return serverSnapshot{ID: id, Status: statusDisconnected}
}

func (p *pool) reconnect(ctx context.Context, id string) (serverSnapshot, error) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	old := p.getEntry(id)
	if old == nil {
		return serverSnapshot{ID: id, Status: statusError, Err: "unknown server"}, transportErr("server not found: %s", id)
	}
	spec := old.spec
	var lastErr error
	for attempt := 0; attempt <= p.maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return old.snapshot(), classify(ctx.Err())
			case <-time.After(p.backoff(attempt)):
			}
		}
		c, err := p.dialFn(ctx, spec)
		if err != nil {
			lastErr = err
			if te, ok := err.(*MCPTransportError); ok && !te.Retryable {
				break
			}
			continue
		}
		ent := &serverEntry{id: id, spec: spec, conn: c, status: statusConnected, createdAt: p.now(), lastPing: p.now()}
		p.refreshCaps(ctx, ent)
		if old.conn != nil {
			_ = old.conn.close()
		}
		p.putEntry(ent)
		return ent.snapshot(), nil
	}
	p.mu.Lock()
	old.status = statusError
	if lastErr != nil {
		old.errMsg = lastErr.Error()
	}
	old.failures++
	snap := old.snapshot()
	p.mu.Unlock()
	return snap, classify(lastErr)
}

func (p *pool) backoff(attempt int) time.Duration {
	d := min(p.base*time.Duration(1<<uint(attempt-1)), p.maxWait)
	p.rngMu.Lock()
	jitter := time.Duration(p.rng.Float64() * 0.2 * float64(d))
	p.rngMu.Unlock()
	return d + jitter
}

func (p *pool) callTool(ctx context.Context, id, tool string, args any) (*mcpsdk.CallToolResult, error) {
	p.mu.RLock()
	ent := p.entries[id]
	connected := ent != nil && ent.status == statusConnected && ent.conn != nil
	var c mcpConn
	if ent != nil {
		c = ent.conn
	}
	p.mu.RUnlock()
	if !connected {
		return nil, transportErr("server not connected: %s", id)
	}
	return c.callTool(ctx, tool, args)
}

func (p *pool) connOf(id string) (mcpConn, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e := p.entries[id]
	if e == nil || e.status != statusConnected || e.conn == nil {
		return nil, false
	}
	return e.conn, true
}

func (p *pool) getPrompt(ctx context.Context, id, name string, args map[string]string) (*mcpsdk.GetPromptResult, error) {
	c, ok := p.connOf(id)
	if !ok {
		return nil, transportErr("server not connected: %s", id)
	}
	return c.getPrompt(ctx, name, args)
}

func (p *pool) readResource(ctx context.Context, id, uri string) (*mcpsdk.ReadResourceResult, error) {
	c, ok := p.connOf(id)
	if !ok {
		return nil, transportErr("server not connected: %s", id)
	}
	return c.readResource(ctx, uri)
}

func (p *pool) promptsOf(id string) []*mcpsdk.Prompt {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.entries[id]; e != nil {
		return e.prompts
	}
	return nil
}

func (p *pool) resourcesOf(id string) []*mcpsdk.Resource {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if e := p.entries[id]; e != nil {
		return e.resources
	}
	return nil
}

func (p *pool) live() []liveServer {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]liveServer, 0, len(p.entries))
	for _, e := range p.entries {
		if e.status == statusConnected || len(e.tools) > 0 {
			out = append(out, liveServer{
				id: e.id, tools: e.tools,
				hasPrompts: len(e.prompts) > 0, hasResources: len(e.resources) > 0,
			})
		}
	}
	return out
}

func (p *pool) get(id string) (*serverEntry, bool) {
	ent := p.getEntry(id)
	return ent, ent != nil
}

func (p *pool) evictOldestUserConn(sep string, max int) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	p.mu.RLock()
	count := 0
	var oldestID string
	var oldestAt time.Time
	for id, e := range p.entries {
		if !strings.Contains(id, sep) {
			continue
		}
		count++
		if oldestID == "" || e.createdAt.Before(oldestAt) {
			oldestID, oldestAt = id, e.createdAt
		}
	}
	p.mu.RUnlock()
	if count < max || oldestID == "" {
		return
	}
	if ent := p.getEntry(oldestID); ent != nil && ent.conn != nil {
		_ = ent.conn.close()
	}
	p.mu.Lock()
	delete(p.entries, oldestID)
	p.mu.Unlock()
}

func (p *pool) servers() []serverSnapshot {
	p.mu.RLock()
	defer p.mu.RUnlock()
	out := make([]serverSnapshot, 0, len(p.entries))
	for _, e := range p.entries {
		out = append(out, e.snapshot())
	}
	return out
}

func (p *pool) healthCheck(ctx context.Context) []string {
	p.mu.RLock()
	ids := make([]string, 0, len(p.entries))
	for id := range p.entries {
		ids = append(ids, id)
	}
	p.mu.RUnlock()

	var failed []string
	for _, id := range ids {
		ent := p.getEntry(id)
		if ent == nil || ent.conn == nil {
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := ent.conn.ping(pctx)
		cancel()
		p.mu.Lock()
		if err != nil {
			ent.status = statusError
			ent.errMsg = err.Error()
		} else {
			ent.status = statusConnected
			ent.lastPing = p.now()
		}
		p.mu.Unlock()
		if err != nil {
			failed = append(failed, id)
		}
	}
	return failed
}

func (p *pool) shutdown(_ context.Context) {
	p.connMu.Lock()
	defer p.connMu.Unlock()
	p.mu.Lock()
	conns := make([]mcpConn, 0, len(p.entries))
	for _, e := range p.entries {
		if e.conn != nil {
			conns = append(conns, e.conn)
		}
	}
	p.entries = map[string]*serverEntry{}
	p.mu.Unlock()

	var wg sync.WaitGroup
	for _, c := range conns {
		wg.Add(1)
		go func(c mcpConn) { defer wg.Done(); _ = c.close() }(c)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
	}
}
