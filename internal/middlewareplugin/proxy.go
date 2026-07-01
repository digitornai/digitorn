package middlewareplugin

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc"

	"github.com/digitornai/digitorn/internal/module/service"
	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/worker"
)

// Picker is the minimal surface needed from a worker pool : hand a healthy
// connection for a kind. *worker.Manager satisfies it ; tests mock it.
type Picker interface {
	Pick(ctx context.Context, kind worker.Kind) (worker.Conn, error)
}

// Proxy is a ports.AppMiddleware that forwards Before/After to an
// out-of-process plugin via the generic worker gRPC service. One Proxy per
// `custom` middleware entry.
type Proxy struct {
	name     string
	moduleID string
	kind     worker.Kind
	picker   Picker
	timeout  time.Duration
	failOpen bool
	logger   *slog.Logger
}

// Options configures a Proxy.
type Options struct {
	Name     string        // middleware name (for logs / pipeline.Names)
	ModuleID string        // module id the worker hosts
	Kind     worker.Kind   // worker pool to dispatch to
	Picker   Picker        // required
	Timeout  time.Duration // per-call cap (0 = 5s)
	FailOpen bool          // on transport error : true = degrade gracefully, false = fail the turn
	Logger   *slog.Logger
}

// New builds a Proxy. The worker pool must already host ModuleID with the
// before/after tools (declared in workers.pools).
func New(opts Options) (*Proxy, error) {
	if opts.ModuleID == "" || opts.Kind == "" {
		return nil, fmt.Errorf("middlewareplugin: module and kind are required")
	}
	if opts.Picker == nil {
		return nil, fmt.Errorf("middlewareplugin: nil picker")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	name := opts.Name
	if name == "" {
		name = "custom:" + opts.ModuleID
	}
	return &Proxy{
		name: name, moduleID: opts.ModuleID, kind: opts.Kind,
		picker: opts.Picker, timeout: timeout, failOpen: opts.FailOpen, logger: logger,
	}, nil
}

func (p *Proxy) Name() string { return p.name }

func (p *Proxy) Before(ctx context.Context, mctx *ports.MiddlewareContext) (string, bool, error) {
	var res BeforeResult
	if err := p.call(ctx, ToolBefore, BeforeRequest{Context: contextFromPorts(mctx)}, &res); err != nil {
		if p.failOpen {
			p.logger.Warn("middleware_plugin_before_failed_open", slog.String("mw", p.name), slog.String("err", err.Error()))
			return "", false, nil
		}
		return "", false, err
	}
	// Apply ONLY what the plugin actually set. A plugin that merely inspects or
	// short-circuits returns a zero BeforeResult ; overwriting unconditionally
	// would clobber the real system prompt with "" and wipe the message history
	// (empty == "leave unchanged", not "clear").
	if res.SystemPrompt != "" {
		mctx.SystemPrompt = res.SystemPrompt
	}
	if len(res.Messages) > 0 {
		mctx.Messages = messagesToPorts(res.Messages)
	}
	return res.Response, res.ShortCircuit, nil
}

func (p *Proxy) After(ctx context.Context, mctx *ports.MiddlewareContext, response string, toolCalls []ports.LLMToolCall) (string, error) {
	var res AfterResult
	req := AfterRequest{Context: contextFromPorts(mctx), Response: response, ToolCalls: toolCallsFromPorts(toolCalls)}
	if err := p.call(ctx, ToolAfter, req, &res); err != nil {
		if p.failOpen {
			p.logger.Warn("middleware_plugin_after_failed_open", slog.String("mw", p.name), slog.String("err", err.Error()))
			return response, nil
		}
		return response, err
	}
	return res.Response, nil
}

// call does one Invoke RPC against the plugin worker, bounded by the timeout,
// and decodes tool.Result.Data into out.
func (p *Proxy) call(ctx context.Context, toolName string, req any, out any) error {
	if p.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, p.timeout)
		defer cancel()
	}
	conn, err := p.picker.Pick(ctx, p.kind)
	if err != nil {
		return fmt.Errorf("pick worker %q: %w", p.kind, err)
	}
	params, err := json.Marshal(req)
	if err != nil {
		return err
	}
	ir := &service.InvokeRequest{ModuleID: p.moduleID, ToolName: toolName, Params: params}
	if dl, ok := ctx.Deadline(); ok {
		ir.DeadlineMs = time.Until(dl).Milliseconds()
	}
	ov := new(service.InvokeResponse)
	if err := conn.GRPC().Invoke(ctx,
		"/"+service.ServiceName+"/"+service.MethodInvoke,
		ir, ov, grpc.CallContentSubtype(service.CodecName),
	); err != nil {
		return fmt.Errorf("invoke %s.%s: %w", p.moduleID, toolName, err)
	}
	if !ov.Result.Success {
		return fmt.Errorf("plugin %s.%s: %s", p.moduleID, toolName, ov.Result.Error)
	}
	b, err := json.Marshal(ov.Result.Data)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}

var _ ports.AppMiddleware = (*Proxy)(nil)
