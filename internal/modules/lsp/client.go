package lsp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

type client struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	writeM sync.Mutex

	nextID atomic.Int64

	mu      sync.Mutex
	pending map[int64]chan rpcResult
	closed  bool

	onNotify func(method string, params json.RawMessage)

	replyGate chan struct{}

	done chan struct{}
}

type rpcResult struct {
	result json.RawMessage
	err    error
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string { return fmt.Sprintf("lsp rpc error %d: %s", e.Code, e.Message) }

func startClient(ctx context.Context, argv []string, cwd string, onNotify func(method string, params json.RawMessage)) (*client, error) {
	if len(argv) == 0 {
		return nil, errors.New("lsp: empty server command")
	}
	exe, err := exec.LookPath(argv[0])
	if err != nil {
		return nil, fmt.Errorf("lsp: server %q not found on PATH: %w", argv[0], err)
	}
	cmd := exec.CommandContext(ctx, exe, argv[1:]...)
	cmd.Dir = cwd
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lsp: start %q: %w", exe, err)
	}
	c := newClientConn(stdin, stdout, onNotify)
	c.cmd = cmd
	return c, nil
}

func newClientConn(stdin io.WriteCloser, stdout io.Reader, onNotify func(method string, params json.RawMessage)) *client {
	c := &client{
		stdin:     stdin,
		pending:   make(map[int64]chan rpcResult),
		onNotify:  onNotify,
		replyGate: make(chan struct{}, 64),
		done:      make(chan struct{}),
	}
	go c.readLoop(stdout)
	return c
}

func (c *client) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.nextID.Add(1)
	ch := make(chan rpcResult, 1)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, errors.New("lsp: client closed")
	}
	c.pending[id] = ch
	c.mu.Unlock()

	if err := c.writeFrame(rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: params}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.done:
		return nil, errors.New("lsp: connection closed before response")
	case r := <-ch:
		return r.result, r.err
	}
}

func (c *client) notify(method string, params any) error {
	return c.writeFrame(rpcRequest{JSONRPC: "2.0", Method: method, Params: params})
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

func (c *client) writeFrame(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	c.writeM.Lock()
	defer c.writeM.Unlock()
	if _, err := io.WriteString(c.stdin, "Content-Length: "+strconv.Itoa(len(b))+"\r\n\r\n"); err != nil {
		return err
	}
	_, err = c.stdin.Write(b)
	return err
}

type incoming struct {
	ID     *json.RawMessage `json:"id"`
	Method string           `json:"method"`
	Params json.RawMessage  `json:"params"`
	Result json.RawMessage  `json:"result"`
	Error  *rpcError        `json:"error"`
}

func (c *client) readLoop(stdout io.Reader) {
	defer close(c.done)
	defer c.failPending(errors.New("lsp: connection closed"))
	r := bufio.NewReader(stdout)
	for {
		frame, err := readFrame(r)
		if err != nil {
			return
		}
		safego.Run("lsp.readLoop", func() {
			var msg incoming
			if err := json.Unmarshal(frame, &msg); err != nil {
				return
			}
			switch {
			case msg.ID != nil && (msg.Result != nil || msg.Error != nil):
				c.deliver(msg)
			case msg.ID != nil && msg.Method != "":
				idCopy := append(json.RawMessage(nil), *msg.ID...)
				select {
				case c.replyGate <- struct{}{}:
					safego.Go("lsp.replyNull", func() {
						defer func() { <-c.replyGate }()
						c.replyNull(idCopy)
					})
				default:
				}
			case msg.Method != "":
				if c.onNotify != nil {
					c.onNotify(msg.Method, msg.Params)
				}
			}
		})
	}
}

func (c *client) deliver(msg incoming) {
	id, err := strconv.ParseInt(strings.TrimSpace(string(*msg.ID)), 10, 64)
	if err != nil {
		return
	}
	c.mu.Lock()
	ch := c.pending[id]
	delete(c.pending, id)
	c.mu.Unlock()
	if ch == nil {
		return
	}
	if msg.Error != nil {
		ch <- rpcResult{err: msg.Error}
		return
	}
	ch <- rpcResult{result: msg.Result}
}

func (c *client) replyNull(id json.RawMessage) {
	_ = c.writeFrame(struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Result  any             `json:"result"`
	}{JSONRPC: "2.0", ID: id, Result: nil})
}

func (c *client) failPending(err error) {
	c.mu.Lock()
	c.closed = true
	for id, ch := range c.pending {
		ch <- rpcResult{err: err}
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *client) close(ctx context.Context) {
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
	}

	politeDone := make(chan struct{})
	go func() {
		defer close(politeDone)
		_, _ = c.call(ctx, "shutdown", nil)
		_ = c.notify("exit", nil)
	}()
	select {
	case <-politeDone:
	case <-ctx.Done():
	}

	_ = c.stdin.Close()
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}

	if c.cmd != nil {
		waitDone := make(chan struct{})
		go func() {
			defer close(waitDone)
			_ = c.cmd.Wait()
		}()
		select {
		case <-waitDone:
		case <-ctx.Done():
		}
	}
	select {
	case <-c.done:
	case <-ctx.Done():
	}
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	n := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "content-length") {
			n, err = strconv.Atoi(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("lsp: bad Content-Length %q", v)
			}
		}
	}
	if n < 0 {
		return nil, errors.New("lsp: frame missing Content-Length")
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}
