package toolmw

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
)

// circuitBreaker protects against cascading failures from an unhealthy module.
// State is app-global (shared across all sessions of the app+module) because a
// dead module is dead for everyone — per-session breakers would let every new
// session re-hammer a known-dead backend.
//
// A "failure" is a transport/framework error (err != nil). A module-level
// Result.Success == false (err == nil) is a normal deterministic answer, not a
// health signal, so it never trips the breaker.
type circuitBreaker struct {
	failureThreshold int
	recoveryTimeout  time.Duration
	halfOpenCalls    int
	logger           *slog.Logger

	mu    sync.Mutex
	state map[string]*breakerState
}

type breakerState struct {
	status     string // closed | open | half_open
	failures   int
	openedAt   time.Time
	halfOpenOK int
}

const (
	cbClosed   = "closed"
	cbOpen     = "open"
	cbHalfOpen = "half_open"
)

func newCircuitBreaker(cfg map[string]any, deps Deps) (Middleware, error) {
	return &circuitBreaker{
		failureThreshold: cfgInt(cfg, "failure_threshold", 3),
		recoveryTimeout:  secs(cfgFloat(cfg, "recovery_timeout", 60.0)),
		halfOpenCalls:    cfgInt(cfg, "half_open_calls", 1),
		logger:           deps.Logger,
		state:            map[string]*breakerState{},
	}, nil
}

func (c *circuitBreaker) Name() string { return "circuit_breaker" }

func (c *circuitBreaker) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	key := cc.ModuleID

	c.mu.Lock()
	st := c.state[key]
	if st == nil {
		st = &breakerState{status: cbClosed}
		c.state[key] = st
	}
	if st.status == cbOpen {
		if time.Since(st.openedAt) < c.recoveryTimeout {
			remaining := c.recoveryTimeout - time.Since(st.openedAt)
			c.mu.Unlock()
			return tool.Result{Success: false, Error: fmt.Sprintf("circuit breaker open for %q, retry in %s", key, remaining.Round(time.Second))},
				fmt.Errorf("toolmw: circuit open for %q", key)
		}
		st.status = cbHalfOpen
		st.halfOpenOK = 0
		if c.logger != nil {
			c.logger.Info("circuit_breaker_half_open", slog.String("module", key))
		}
	}
	wasHalfOpen := st.status == cbHalfOpen
	c.mu.Unlock()

	res, err := next(ctx, cc)

	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		st.failures++
		if st.failures >= c.failureThreshold {
			st.status = cbOpen
			st.openedAt = time.Now()
			if c.logger != nil {
				c.logger.Warn("circuit_breaker_open", slog.String("module", key), slog.Int("failures", st.failures))
			}
		}
		return res, err
	}
	if wasHalfOpen {
		st.halfOpenOK++
		if st.halfOpenOK >= c.halfOpenCalls {
			st.status = cbClosed
			st.failures = 0
			if c.logger != nil {
				c.logger.Info("circuit_breaker_closed", slog.String("module", key))
			}
		}
	} else {
		st.failures = 0
	}
	return res, nil
}
