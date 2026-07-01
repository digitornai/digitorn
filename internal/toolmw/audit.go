package toolmw

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// audit logs every tool call with timing and outcome, and keeps lock-free
// counters for a future /metrics surface.
type audit struct {
	logParams bool
	logResult bool
	logger    *slog.Logger

	calls      atomic.Uint64
	errors     atomic.Uint64
	durationMs atomic.Uint64
}

func newAudit(cfg map[string]any, deps Deps) (Middleware, error) {
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &audit{
		logParams: cfgBool(cfg, "log_params", false),
		logResult: cfgBool(cfg, "log_result", false),
		logger:    logger,
	}, nil
}

func (a *audit) Name() string { return "audit" }

func (a *audit) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	a.calls.Add(1)
	start := time.Now()
	res, err := next(ctx, cc)
	dur := time.Since(start)
	a.durationMs.Add(uint64(dur.Milliseconds()))

	attrs := []any{
		slog.String("module", cc.ModuleID), slog.String("tool", cc.ToolName),
		slog.String("session", cc.SessionID), slog.Int64("duration_ms", dur.Milliseconds()),
		slog.Bool("ok", err == nil && res.Success),
	}
	if a.logParams {
		attrs = append(attrs, slog.String("params", string(cc.Params)))
	}
	if err != nil {
		a.errors.Add(1)
		attrs = append(attrs, slog.String("err", err.Error()))
	} else if !res.Success {
		a.errors.Add(1)
		attrs = append(attrs, slog.String("tool_error", res.Error))
	} else if a.logResult {
		attrs = append(attrs, slog.String("result", truncate(renderResult(res), 200)))
	}
	a.logger.Info("tool_audit", attrs...)
	return res, err
}

func renderResult(res tool.Result) string {
	switch v := res.Data.(type) {
	case string:
		return v
	case []byte:
		return string(v)
	}
	return ""
}

func truncate(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	// Back up to a rune boundary so an audit value is never cut mid-rune
	// (a raw s[:n] can emit invalid UTF-8 into the audit row / JSON).
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
