package pieces

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

const (
	initTimeout = 15 * time.Second
	callTimeout = 60 * time.Second
)

type rpcMsg struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	result json.RawMessage
	err    *rpcError
}

// Bridge manages the bridge subprocess and speaks MCP JSON-RPC over stdio.
type Bridge struct {
	mu          sync.RWMutex
	cmd         *exec.Cmd
	stdin       io.WriteCloser
	encMu       sync.Mutex
	enc         *json.Encoder
	pending     sync.Map // int64 → chan rpcResponse
	idGen       atomic.Int64
	cachedTools []tool.Spec
	toolsAt     time.Time
	toolsMu     sync.Mutex // guards cachedTools/toolsAt only — never held across a bridge call
	refreshMu   sync.Mutex // single-flight: at most one tools/list refresh in flight
	running     atomic.Bool

	// config
	bridgePath  string
	piecesDir   string
	triggerPort int
	extraEnv    []string
}

func newBridge(bridgePath, piecesDir string, triggerPort int, extraEnv []string) *Bridge {
	return &Bridge{
		bridgePath:  bridgePath,
		piecesDir:   piecesDir,
		triggerPort: triggerPort,
		extraEnv:    extraEnv,
	}
}

func (b *Bridge) Start(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.running.Load() {
		return nil
	}
	return b.startLocked(ctx)
}

func (b *Bridge) startLocked(ctx context.Context) error {
	if _, err := os.Stat(b.bridgePath); err != nil {
		return fmt.Errorf("bridge binary not found at %q: %w", b.bridgePath, err)
	}

	env := append(os.Environ(), b.extraEnv...)
	env = append(env,
		"DIGITORN_PIECES_DIR="+b.piecesDir,
		fmt.Sprintf("DIGITORN_AP_TRIGGER_PORT=%d", b.triggerPort),
	)

	cmd := exec.CommandContext(context.Background(), b.bridgePath)
	cmd.Env = env
	cmd.Stderr = os.Stderr

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("bridge stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		stdin.Close()
		return fmt.Errorf("bridge stdout pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		stdin.Close()
		return fmt.Errorf("bridge start: %w", err)
	}

	b.cmd = cmd
	b.stdin = stdin
	b.enc = json.NewEncoder(stdin)
	b.running.Store(true)

	go b.readLoop(stdout)

	// MCP initialize handshake.
	initCtx, cancel := context.WithTimeout(context.Background(), initTimeout)
	defer cancel()
	if err := b.initialize(initCtx); err != nil {
		b.stopLocked()
		return fmt.Errorf("bridge initialize: %w", err)
	}
	return nil
}

func (b *Bridge) stopLocked() {
	b.running.Store(false)
	if b.stdin != nil {
		b.stdin.Close()
	}
	if b.cmd != nil && b.cmd.Process != nil {
		b.cmd.Process.Kill()
		b.cmd.Wait()
	}
	b.cmd = nil
	b.stdin = nil
	b.enc = nil
}

func (b *Bridge) Stop(ctx context.Context) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
}

func (b *Bridge) Restart(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.stopLocked()
	b.toolsMu.Lock()
	b.cachedTools = nil
	b.toolsMu.Unlock()
	return b.startLocked(ctx)
}

func (b *Bridge) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4<<20), 4<<20)
	for scanner.Scan() { //nolint:revive
		line := scanner.Bytes()
		var msg rpcMsg
		if err := json.Unmarshal(line, &msg); err != nil {
			continue
		}
		if msg.ID == nil {
			continue // notification
		}
		id, ok := toInt64(msg.ID)
		if !ok {
			continue
		}
		ch, loaded := b.pending.LoadAndDelete(id)
		if !loaded {
			continue
		}
		ch.(chan rpcResponse) <- rpcResponse{result: msg.Result, err: msg.Error}
	}
	b.running.Store(false)
}

func (b *Bridge) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	if !b.running.Load() {
		return nil, fmt.Errorf("bridge not running")
	}
	id := b.idGen.Add(1)
	ch := make(chan rpcResponse, 1)
	b.pending.Store(id, ch)

	raw, _ := json.Marshal(params)
	msg := rpcMsg{JSONRPC: "2.0", ID: id, Method: method, Params: raw}

	b.encMu.Lock()
	err := b.enc.Encode(msg)
	b.encMu.Unlock()
	if err != nil {
		b.pending.Delete(id)
		return nil, fmt.Errorf("bridge write: %w", err)
	}

	select {
	case resp := <-ch:
		if resp.err != nil {
			return nil, fmt.Errorf("bridge error %d: %s", resp.err.Code, resp.err.Message)
		}
		return resp.result, nil
	case <-ctx.Done():
		b.pending.Delete(id)
		return nil, ctx.Err()
	}
}

func (b *Bridge) notify(method string, params any) error {
	if !b.running.Load() {
		return fmt.Errorf("bridge not running")
	}
	raw, _ := json.Marshal(params)
	msg := rpcMsg{JSONRPC: "2.0", Method: method, Params: raw}
	b.encMu.Lock()
	defer b.encMu.Unlock()
	return b.enc.Encode(msg)
}

func (b *Bridge) initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "digitorn", "version": "1.0"},
	}
	if _, err := b.call(ctx, "initialize", params); err != nil {
		return err
	}
	return b.notify("notifications/initialized", map[string]any{})
}

// ListTools returns the bridge's tool list, cached for 5 minutes.
//
// The slow part — the bridge tools/list round-trip — runs WITHOUT holding any
// lock the agent loop needs. toolsMu only ever guards the brief cache read/swap.
// When the cache is stale, a single goroutine (guarded by refreshMu) refreshes
// while every other caller is served the stale snapshot immediately, so a slow
// or hung refresh can never stall agent turns that just need the tool list.
func (b *Bridge) ListTools(ctx context.Context) ([]tool.Spec, error) {
	cached, at := b.snapshotTools()
	if cached != nil && time.Since(at) < 5*time.Minute {
		return cached, nil
	}

	// Stale or empty: try to become the sole refresher.
	if !b.refreshMu.TryLock() {
		if cached != nil {
			return cached, nil // another goroutine is refreshing — serve stale
		}
		// Cold start, no cache yet: wait for the in-flight refresh, then read.
		b.refreshMu.Lock()
		b.refreshMu.Unlock()
		cached, _ = b.snapshotTools()
		return cached, nil
	}
	defer b.refreshMu.Unlock()

	// Another goroutine may have refreshed between our snapshot and the lock.
	if cached, at = b.snapshotTools(); cached != nil && time.Since(at) < 5*time.Minute {
		return cached, nil
	}

	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	raw, err := b.call(callCtx, "tools/list", map[string]any{})
	if err != nil {
		return cached, err // return stale on error
	}

	var resp struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return b.cachedTools, fmt.Errorf("bridge tools/list parse: %w", err)
	}

	specs := make([]tool.Spec, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		risk, irreversible := inferRisk(t.Name)
		specs = append(specs, tool.Spec{
			Name:         t.Name,
			Description:  t.Description,
			Params:       schemaToParams(t.InputSchema),
			RiskLevel:    risk,
			Irreversible: irreversible,
			Tags:         []string{"pieces", pieceTagOf(t.Name)},
		})
	}
	b.toolsMu.Lock()
	b.cachedTools = specs
	b.toolsAt = time.Now()
	b.toolsMu.Unlock()
	return specs, nil
}

// snapshotTools reads the cached tool list and its timestamp under toolsMu
// (held only for this read — never across a bridge call).
func (b *Bridge) snapshotTools() ([]tool.Spec, time.Time) {
	b.toolsMu.Lock()
	defer b.toolsMu.Unlock()
	return b.cachedTools, b.toolsAt
}

// CallTool invokes a piece action via the bridge and returns a tool.Result.
func (b *Bridge) CallTool(ctx context.Context, name string, args json.RawMessage) (tool.Result, error) {
	callCtx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()

	var argsMap map[string]any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &argsMap)
	}
	if argsMap == nil {
		argsMap = map[string]any{}
	}

	params := map[string]any{"name": name, "arguments": argsMap}
	raw, err := b.call(callCtx, "tools/call", params)
	if err != nil {
		return tool.Result{Success: false, Error: err.Error()}, nil
	}

	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return tool.Result{Success: false, Error: "bridge response parse: " + err.Error()}, nil
	}

	if resp.IsError {
		msg := "piece action failed"
		if len(resp.Content) > 0 {
			msg = resp.Content[0].Text
		}
		return tool.Result{Success: false, Error: msg}, nil
	}

	// Parse the JSON text payload returned by the bridge executor.
	if len(resp.Content) == 0 {
		return tool.Result{Success: true}, nil
	}
	var payload struct {
		OK   bool   `json:"ok"`
		Data any    `json:"data"`
		Err  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(resp.Content[0].Text), &payload); err != nil {
		return tool.Result{Success: true, Data: map[string]any{"text": resp.Content[0].Text}}, nil
	}
	if !payload.OK {
		return tool.Result{Success: false, Error: payload.Err}, nil
	}
	return tool.Result{Success: true, Data: payload.Data}, nil
}

func toInt64(v any) (int64, bool) {
	switch x := v.(type) {
	case float64:
		return int64(x), true
	case int64:
		return x, true
	case json.Number:
		n, err := x.Int64()
		return n, err == nil
	}
	return 0, false
}

// ── Trigger server HTTP client ────────────────────────────────────────

// TriggerPort returns the trigger HTTP server port so external callers
// (e.g. the background adapter) can call /trigger/poll directly.
func (b *Bridge) TriggerPort() int { return b.triggerPort }

// triggerHTTPPost calls the bridge's trigger HTTP server with a JSON body.
func (b *Bridge) triggerHTTPPost(path string, body any) ([]byte, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", b.triggerPort, path)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trigger server HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// TriggerPollRequest is the body sent to /trigger/poll.
type TriggerPollRequest struct {
	Piece      string            `json:"piece"`
	Trigger    string            `json:"trigger"`
	Auth       any               `json:"auth"`
	Props      map[string]any    `json:"props"`
	StoreState map[string]any    `json:"storeState"`
	WebhookURL string            `json:"webhookUrl,omitempty"`
}

// TriggerPollResponse is the response from /trigger/poll.
type TriggerPollResponse struct {
	Events     []map[string]any `json:"events"`
	StoreState map[string]any   `json:"storeState"`
}

// TriggerPoll calls /trigger/poll on the bridge trigger server.
func (b *Bridge) TriggerPoll(req TriggerPollRequest) (TriggerPollResponse, error) {
	data, err := b.triggerHTTPPost("/trigger/poll", req)
	if err != nil {
		return TriggerPollResponse{}, err
	}
	var resp TriggerPollResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		return TriggerPollResponse{}, err
	}
	return resp, nil
}

// TriggerEnable calls /trigger/enable on the bridge trigger server.
func (b *Bridge) TriggerEnable(req TriggerPollRequest) error {
	_, err := b.triggerHTTPPost("/trigger/enable", req)
	return err
}

// triggerHTTPGet calls the bridge's trigger HTTP server.
func (b *Bridge) triggerHTTPGet(path string) ([]byte, error) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", b.triggerPort, path)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("trigger server HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

// GetPieceAuth returns the auth schema for a piece from the bridge.
func (b *Bridge) GetPieceAuth(pieceName string) (map[string]any, error) {
	data, err := b.triggerHTTPGet("/pieces/" + pieceName + "/auth")
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetPieceStatus returns piece status from the bridge.
func (b *Bridge) GetPieceStatus(pieceName string) (map[string]any, error) {
	data, err := b.triggerHTTPGet("/pieces/" + pieceName + "/status")
	if err != nil {
		return nil, err
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, err
	}
	return result, nil
}
