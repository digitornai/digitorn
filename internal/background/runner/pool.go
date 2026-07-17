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

type Processor interface {
	Process(ctx context.Context, job store.Job) error
}

type Retryable struct {
	Err   error
	After time.Duration
}

func (r *Retryable) Error() string { return r.Err.Error() }
func (r *Retryable) Unwrap() error { return r.Err }

func Retry(err error, after time.Duration) error { return &Retryable{Err: err, After: after} }

type Options struct {
	Workers  int
	LeaseTTL time.Duration
	PollMin  time.Duration
	PollMax  time.Duration
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

type Pool struct {
	store *store.Store
	proc  Processor
	opt   Options

	processed atomic.Int64
	completed atomic.Int64
	retried   atomic.Int64
	failed    atomic.Int64
}

func New(st *store.Store, proc Processor, opt Options) *Pool {
	opt.withDefaults()
	return &Pool{store: st, proc: proc, opt: opt}
}

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
				close(jobs)
				wg.Wait()
				return
			}
		}
	}
	close(jobs)
	wg.Wait()
}

func (p *Pool) handle(ctx context.Context, j store.Job) {
	p.processed.Add(1)
	err := p.safeProcess(ctx, j)
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
			err = Retry(errors.New("processor panic"), 5*time.Second)
		}
	}()
	return p.proc.Process(ctx, j)
}

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
