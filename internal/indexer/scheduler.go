package indexer

import (
	"context"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
)

// scheduler drives every source-sync job (on_start / interval / cron / manual)
// for ALL apps through ONE dispatcher goroutine and a FIXED worker pool : 100k
// apps cost a handful of goroutines + a fixed concurrency cap, not 100k
// tickers. A job that is due but finds no slot stays due and runs as soon as
// one frees — never dropped. PER-APP FAIRNESS : each owner (app) may hold at
// most workers/activeOwners slots at a time, so one app with 10k sources can
// never starve the others ; when a single app is active it still uses the full
// pool. Each job is single-flight.
type scheduler struct {
	mu      sync.Mutex
	jobs    map[string]*schedJob
	byOwner map[string]int
	workers int
	queue   chan *schedJob
	wake    chan struct{}
	stop    chan struct{}
	started bool
	stopped bool
	tick    time.Duration
	metrics *Metrics
	wg      sync.WaitGroup
}

type schedJob struct {
	key     string
	owner   string
	at      time.Time
	due     bool
	running bool
	next    func(time.Time) time.Time // nil = no periodic schedule (trigger-only)
	fn      func(context.Context) error
}

func newScheduler(maxConcurrent int, tick time.Duration) *scheduler {
	if maxConcurrent <= 0 {
		maxConcurrent = 8
	}
	if tick <= 0 {
		tick = 15 * time.Second
	}
	return &scheduler{
		jobs:    map[string]*schedJob{},
		byOwner: map[string]int{},
		workers: maxConcurrent,
		queue:   make(chan *schedJob),
		wake:    make(chan struct{}, 1),
		stop:    make(chan struct{}),
		tick:    tick,
		metrics: newMetrics(),
	}
}

func intervalNext(d time.Duration) func(time.Time) time.Time {
	return func(t time.Time) time.Time { return t.Add(d) }
}

func cronNext(expr string) func(time.Time) time.Time {
	sched, err := cron.ParseStandard(expr)
	if err != nil {
		return nil
	}
	return func(t time.Time) time.Time { return sched.Next(t) }
}

func (s *scheduler) startLocked() {
	if s.started {
		return
	}
	s.started = true
	for i := 0; i < s.workers; i++ {
		go s.worker()
	}
	go s.dispatcher()
}

func (s *scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *scheduler) register(key, owner string, next func(time.Time) time.Time, fn func(context.Context) error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return
	}
	j := s.jobs[key]
	if j == nil {
		j = &schedJob{key: key}
		s.jobs[key] = j
	}
	j.owner = owner
	j.fn = fn
	j.next = next
	if next != nil {
		j.at = next(time.Now())
	} else {
		j.at = time.Time{}
	}
	s.startLocked()
}

func (s *scheduler) deregister(key string) {
	s.mu.Lock()
	delete(s.jobs, key)
	s.mu.Unlock()
}

func (s *scheduler) trigger(key string) {
	s.mu.Lock()
	if j := s.jobs[key]; j != nil && !s.stopped {
		j.due = true
		s.startLocked()
	}
	s.mu.Unlock()
	s.signal()
}

// fairShareLocked returns the per-owner in-flight cap : the pool split evenly
// across every owner that currently has work (running or due), floored at 1.
// One active owner → the whole pool ; N owners → ~workers/N each.
func (s *scheduler) fairShareLocked() int {
	active := map[string]struct{}{}
	for _, j := range s.jobs {
		if j.running || j.due {
			active[j.owner] = struct{}{}
		}
	}
	n := len(active)
	if n == 0 {
		n = 1
	}
	share := s.workers / n
	if share < 1 {
		share = 1
	}
	return share
}

func (s *scheduler) dispatcher() {
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		case <-s.wake:
		}

		now := time.Now()
		s.mu.Lock()
		for _, j := range s.jobs {
			if j.next != nil && !j.due && !j.running && !j.at.IsZero() && !now.Before(j.at) {
				j.due = true
			}
		}
		share := s.fairShareLocked()
		ready := make([]*schedJob, 0)
		for _, j := range s.jobs {
			if j.due && !j.running && s.byOwner[j.owner] < share {
				ready = append(ready, j)
			}
		}
		s.mu.Unlock()

		for _, j := range ready {
			s.mu.Lock()
			if j.running || !j.due || s.stopped || s.byOwner[j.owner] >= share {
				s.mu.Unlock()
				continue
			}
			j.running = true
			j.due = false
			s.byOwner[j.owner]++
			s.wg.Add(1)
			s.mu.Unlock()

			select {
			case s.queue <- j:
			case <-s.stop:
				s.mu.Lock()
				j.running = false
				s.decOwnerLocked(j.owner)
				s.mu.Unlock()
				s.wg.Done()
				return
			}
		}
	}
}

func (s *scheduler) decOwnerLocked(owner string) {
	s.byOwner[owner]--
	if s.byOwner[owner] <= 0 {
		delete(s.byOwner, owner)
	}
}

func (s *scheduler) worker() {
	for {
		select {
		case <-s.stop:
			return
		case j := <-s.queue:
			s.runJob(j)
		}
	}
}

func (s *scheduler) runJob(j *schedJob) {
	s.metrics.gaugeAdd(&s.metrics.InFlight, 1)
	defer func() {
		_ = recover()
		s.metrics.gaugeAdd(&s.metrics.InFlight, -1)
		s.mu.Lock()
		j.running = false
		s.decOwnerLocked(j.owner)
		if j.next != nil {
			j.at = j.next(time.Now())
		}
		s.mu.Unlock()
		s.wg.Done()
		s.signal()
	}()

	s.mu.Lock()
	fn := j.fn
	s.mu.Unlock()

	if fn != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		_ = fn(ctx)
	}
}

// shutdown stops the dispatcher + workers and waits for in-flight jobs to
// drain, bounded by ctx. After it returns no new jobs start.
func (s *scheduler) shutdown(ctx context.Context) {
	s.mu.Lock()
	already := s.stopped
	started := s.started
	s.stopped = true
	s.mu.Unlock()
	if already || !started {
		return
	}
	close(s.stop)

	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
