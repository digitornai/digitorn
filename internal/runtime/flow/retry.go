package flow

import (
	"context"
	"fmt"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const (
	defaultRetryBackoff    = 500 * time.Millisecond
	defaultRetryMultiplier = 2.0
	defaultRetryMaxBackoff = 30 * time.Second
)

// execWithRetry runs a node, re-running it on a retryable failure per node.Retry
// with geometric backoff. Between attempts it emits a "retrying" node-end + a
// fresh node-start so the timeline shows the attempts. The final failure (retries
// exhausted / non-retryable) is returned to runPath, where on_error still applies.
func (r *Runner) execWithRetry(ctx context.Context, node schema.FlowNode, fc *fctx, in runInput, iter int) (execResult, error) {
	attempts, base, mult, max := retryParams(node.Retry)
	if attempts <= 1 {
		return r.executeNode(ctx, node, fc, in)
	}
	var (
		res execResult
		err error
	)
	for attempt := 1; ; attempt++ {
		res, err = r.executeNode(ctx, node, fc, in)
		if err == nil || attempt >= attempts || !retryableErr(node.Retry, err) {
			return res, err
		}
		delay := backoffDelay(base, mult, max, attempt)
		r.emit(ctx, in, sessionstore.EventFlowNodeEnd, node.ID, node.Type, "retrying", res.text,
			fmt.Sprintf("attempt %d/%d failed: %s; retrying in %s", attempt, attempts, err.Error(), delay), iter)
		if serr := sleepCtx(ctx, delay); serr != nil {
			return res, err
		}
		r.emit(ctx, in, sessionstore.EventFlowNodeStart, node.ID, node.Type, "running", "", "", iter)
	}
}

// retryParams resolves a node's retry policy to concrete values, applying
// defaults. attempts == 1 means no retry (a single try).
func retryParams(rc *schema.FlowRetry) (attempts int, base time.Duration, mult float64, max time.Duration) {
	if rc == nil || rc.MaxAttempts <= 1 {
		return 1, 0, 0, 0
	}
	attempts = rc.MaxAttempts
	base = defaultRetryBackoff
	if rc.BackoffMs > 0 {
		base = time.Duration(rc.BackoffMs) * time.Millisecond
	}
	mult = defaultRetryMultiplier
	if rc.Multiplier > 0 {
		mult = rc.Multiplier
	}
	max = defaultRetryMaxBackoff
	if rc.MaxBackoffMs > 0 {
		max = time.Duration(rc.MaxBackoffMs) * time.Millisecond
	}
	return attempts, base, mult, max
}

// retryableErr reports whether err should be retried under the policy's Match
// regexp (empty Match = retry any error).
func retryableErr(rc *schema.FlowRetry, err error) bool {
	if err == nil {
		return false
	}
	if rc == nil || rc.Match == "" {
		return true
	}
	re, cerr := cachedRegexp(rc.Match)
	if cerr != nil {
		return true
	}
	return re.MatchString(err.Error())
}

// backoffDelay is base * mult^(attempt-1), capped at max.
func backoffDelay(base time.Duration, mult float64, max time.Duration, attempt int) time.Duration {
	d := float64(base)
	for i := 1; i < attempt; i++ {
		d *= mult
		if time.Duration(d) >= max {
			return max
		}
	}
	if time.Duration(d) > max {
		return max
	}
	return time.Duration(d)
}

// sleepCtx sleeps for d or returns ctx.Err() if the context is cancelled first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
