package toolmw

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/safego"
)

// timeout enforces a per-call deadline. It sets a ctx deadline (so a
// well-behaved module aborts) AND races the call against a timer, so a module
// that ignores ctx cannot hold a turn forever : on expiry we return a timeout
// error and let the orphaned goroutine drain in the background.
type timeout struct {
	d      time.Duration
	logger *slog.Logger
}

func newTimeout(cfg map[string]any, deps Deps) (Middleware, error) {
	return &timeout{d: secs(cfgFloat(cfg, "seconds", 30.0)), logger: deps.Logger}, nil
}

func (t *timeout) Name() string { return "timeout" }

func (t *timeout) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	if t.d <= 0 {
		return next(ctx, cc)
	}
	cctx, cancel := context.WithTimeout(ctx, t.d)
	defer cancel()

	type outcome struct {
		res tool.Result
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		// A module that panics must surface as one errored result on this
		// per-call goroutine, never crash the daemon. done is buffered so the
		// send never blocks even if the timeout case already fired.
		defer func() {
			if r := recover(); r != nil {
				msg := safego.Report("toolmw.timeout:"+cc.ModuleID+"."+cc.ToolName, r)
				done <- outcome{tool.Result{Success: false, Error: msg}, fmt.Errorf("toolmw: %s.%s: %s", cc.ModuleID, cc.ToolName, msg)}
			}
		}()
		res, err := next(cctx, cc)
		done <- outcome{res, err}
	}()

	select {
	case o := <-done:
		return o.res, o.err
	case <-cctx.Done():
		if t.logger != nil {
			t.logger.Warn("tool_middleware_timeout",
				slog.String("module", cc.ModuleID), slog.String("tool", cc.ToolName),
				slog.Duration("timeout", t.d))
		}
		return tool.Result{Success: false, Error: fmt.Sprintf("tool call timed out after %s", t.d)},
			fmt.Errorf("toolmw: %s.%s timed out after %s", cc.ModuleID, cc.ToolName, t.d)
	}
}
