package toolmw

import (
	"context"
	"log/slog"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// retry re-runs a call that failed at the transport level with backoff.
//
// Fix vs the reference, which retried on ANY exception : we retry only on a
// transport/framework error (err != nil — worker died, network glitch). A
// module-level failure (Result.Success == false, err == nil) is deterministic,
// so retrying wastes time and budget ; it is returned immediately. A cancelled
// context also stops retrying at once.
type retry struct {
	maxAttempts int
	baseDelay   time.Duration
	maxDelay    time.Duration
	exponential bool
	logger      *slog.Logger
}

func newRetry(cfg map[string]any, deps Deps) (Middleware, error) {
	r := &retry{
		maxAttempts: cfgInt(cfg, "max_attempts", 3),
		baseDelay:   secs(cfgFloat(cfg, "base_delay", 1.0)),
		maxDelay:    secs(cfgFloat(cfg, "max_delay", 30.0)),
		exponential: cfgStr(cfg, "backoff", "exponential") != "fixed",
		logger:      deps.Logger,
	}
	if r.maxAttempts < 1 {
		r.maxAttempts = 1
	}
	return r, nil
}

func (r *retry) Name() string { return "retry" }

func (r *retry) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	var (
		res tool.Result
		err error
	)
	for attempt := 1; attempt <= r.maxAttempts; attempt++ {
		cc.Attempt = attempt
		res, err = next(ctx, cc)
		if err == nil {
			return res, nil
		}
		if ctx.Err() != nil || attempt == r.maxAttempts {
			return res, err
		}
		delay := r.delay(attempt)
		if r.logger != nil {
			r.logger.Warn("tool_middleware_retry",
				slog.String("module", cc.ModuleID), slog.String("tool", cc.ToolName),
				slog.Int("attempt", attempt), slog.Int("max", r.maxAttempts),
				slog.Duration("delay", delay), slog.String("err", err.Error()))
		}
		select {
		case <-ctx.Done():
			return res, err
		case <-time.After(delay):
		}
	}
	return res, err
}

func (r *retry) delay(attempt int) time.Duration {
	if r.baseDelay <= 0 {
		return 0
	}
	if !r.exponential {
		return min(r.baseDelay, r.maxDelay)
	}
	// base * 2^(attempt-1), clamped to maxDelay. A large shift overflows the
	// int64 (to <= 0 or above the cap), which the bound check below folds back
	// onto maxDelay.
	d := r.baseDelay << (attempt - 1)
	if d <= 0 || d > r.maxDelay {
		return r.maxDelay
	}
	return d
}

func secs(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
