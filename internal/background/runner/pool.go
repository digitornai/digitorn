// Package runner drains the durable job store with a bounded worker pool. It is
// the engine of the background service: claim → process → complete/fail, with
// backpressure, a panic shield (a processor panic can never take the service
// down), and graceful drain on shutdown. Processing itself is injected via the
// Processor seam — BG-3 plugs the real "invoke the daemon" processor; tests plug
// a fake. No daemon coupling.
package runner

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/background/store"
)

// Processor handles one claimed job. Returning nil completes it; returning a
// Retryable schedules a backoff retry; any other error fails it terminally.
type Processor interface {
	Process(ctx context.Context, job store.Job) error
}

// Retryable marks an error as transient: the job goes back to pending, not
// claimable until After elapses.
type Retryable struct {
	Err   error
	After time.Duration
}

func (r *Retryable) Error() string { return r.Err.Error() }
func (r *Retryable) Unwrap() error { return r.Err }

// Retry wraps err as a transient failure to retry after the given delay.
func Retry(err error, after time.Duration) error { return &Retryable{Err: err, After: after} }

// Options configures the pool. Zero values fall back to sane defaults.
type Options struct {
	Workers  int           // concurrent processors (default 8)
	LeaseTTL time.Duration // lease held per job (default 60s)
	PollMin  time.Duration // idle poll floor (default 100ms)
	PollMax  time.Duration // idle poll ceiling (default 2s)
	Logger   *slog.Logger
}

func (o *Options) withDefaults() {
	if o.Workers <= 0 {
		o.Workers = 8
	}
	if o.LeaseTTL <= 0 {
		o.LeaseTTL = 60 * time.Second
	}
	if o.PollMin <= 0 {
		o.PollMin = 100 * time.Millisecond
	}
	if o.PollMax <= 0 {
		o.PollMax = 2 * time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
}

// Pool is the bounded job-draining engine.
type Pool struct {
	store *store.Store
	proc  Processor
	opt   Options

	processed atomic.Int64
	completed atomic.Int64
	retried   atomic.Int64
	failed    atomic.Int64
}

// New builds a pool over the store with the given processor.
func New(st *store.Store, proc Processor, opt Options) *Pool {
	opt.withDefaults()
	return &Pool{store: st, proc: proc, opt: opt}
}

// Stats is a snapshot of pool counters.
type Stats struct {
	Processed int64 `json:"processed"`
	Completed int64 `json:"completed"`
	Retried   int64 `json:"retried"`
	Failed    int64 `json:"failed"`
	Workers   int   `json:"workers"`
}

func (p *Pool) Stats() Stats {
	return Stats{
		Processed: p.processed.Load(),
		Completed: p.completed.Load(),
		Retried:   p.retried.Load(),
		Failed:    p.failed.Load(),
		Workers:   p.opt.Workers,
	}
}

// Run drains jobs until ctx is cancelled, then drains in-flight work and
// returns. A dispatcher goroutine claims batches and feeds a bounded channel;
// pushing blocks when all workers are busy, which is the backpressure (we never
// claim faster than we can process, so leases aren't held by idle queued jobs).
func (p *Pool) Run(ctx context.Context) {
	jobs := make(chan store.Job)
	var wg sync.WaitGroup
	for i := 0; i < p.opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				p.handle(ctx, j)
			}
		}()
	}

	backoff := p.opt.PollMin
	for {
		if ctx.Err() != nil {
			break
		}
		// Claim a worker's worth; the blocking push below regulates the rate.
		batch, err := p.store.Claim(ctx, p.opt.Workers, p.opt.LeaseTTL)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			p.opt.Logger.Warn("background: claim failed", "err", err.Error())
			if !sleep(ctx, backoff) {
				break
			}
			backoff = grow(backoff, p.opt.PollMax)
			continue
		}
		if len(batch) == 0 {
			if !sleep(ctx, backoff) {
				break
			}
			backoff = grow(backoff, p.opt.PollMax)
			continue
		}
		backoff = p.opt.PollMin
		for _, j := range batch {
			select {
			case jobs <- j:
			case <-ctx.Done():
				// Shutdown mid-dispatch: this job stays leased and is re-run after
				// its lease expires (durability). Stop feeding.
				close(jobs)
				wg.Wait()
				return
			}
		}
	}
	close(jobs)
	wg.Wait()
}

// handle runs one job under a panic shield. A panic or error becomes a durable
// Fail (retryable) — the worker and the service keep running no matter what the
// processor does.
func (p *Pool) handle(ctx context.Context, j store.Job) {
	p.processed.Add(1)
	err := p.safeProcess(ctx, j)
	// Use a detached context for the bookkeeping write so a cancelled ctx during
	// shutdown still records the outcome (the write is tiny + local).
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	switch {
	case err == nil:
		if e := p.store.Complete(wctx, j.ID); e != nil {
			p.opt.Logger.Warn("background: complete failed", "job", j.ID, "err", e.Error())
		}
		p.completed.Add(1)
	default:
		var rt *Retryable
		if errors.As(err, &rt) {
			if e := p.store.Fail(wctx, j.ID, rt.Error(), rt.After); e != nil {
				p.opt.Logger.Warn("background: fail(retry) failed", "job", j.ID, "err", e.Error())
			}
			p.retried.Add(1)
		} else {
			if e := p.store.Fail(wctx, j.ID, err.Error(), 0); e != nil {
				p.opt.Logger.Warn("background: fail(terminal) failed", "job", j.ID, "err", e.Error())
			}
			p.failed.Add(1)
		}
	}
}

func (p *Pool) safeProcess(ctx context.Context, j store.Job) (err error) {
	defer func() {
		if r := recover(); r != nil {
			p.opt.Logger.Error("background: processor panicked", "job", j.ID, "panic", r)
			// A panic is treated as transient — retry shortly rather than lose the job.
			err = Retry(errors.New("processor panic"), 5*time.Second)
		}
	}()
	return p.proc.Process(ctx, j)
}

// sleep waits d or until ctx is done; returns false if ctx ended.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func grow(d, max time.Duration) time.Duration {
	d *= 2
	if d > max {
		return max
	}
	return d
}
