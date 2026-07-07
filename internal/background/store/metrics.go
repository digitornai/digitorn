package store

import (
	"context"
	"sort"
	"time"
)

// Percentiles summarises run durations (ms) over a window.
type Percentiles struct {
	P50 int64 `json:"p50"`
	P95 int64 `json:"p95"`
	Max int64 `json:"max"`
}

// Metrics is the background health snapshot for the ops surface: current job
// backlog (Jobs.Failed is the dead-letter depth), run outcomes over a recent
// window, the derived success rate, and duration percentiles.
type Metrics struct {
	AppID       string           `json:"app_id,omitempty"`
	Window      string           `json:"window"`
	Since       time.Time        `json:"since"`
	Jobs        Counts           `json:"jobs"`
	Runs        int64            `json:"runs"`
	ByOutcome   map[string]int64 `json:"by_outcome"`
	OK          int64            `json:"ok"`
	Failed      int64            `json:"failed"`
	SuccessRate float64          `json:"success_rate"`
	DurationMs  Percentiles      `json:"duration_ms"`
}

// metricsRunCap bounds how many run durations are pulled for percentile math so
// a huge history can't blow memory; the newest cap rows are representative.
const metricsRunCap = 5000

// MetricsWindow aggregates job backlog + run outcomes since `since`. appID == ""
// spans all apps. Job counts are current (backlog is a live gauge); run counts,
// success rate and durations are scoped to the window.
func (s *Store) MetricsWindow(ctx context.Context, appID string, since time.Time) (Metrics, error) {
	m := Metrics{AppID: appID, Since: since, ByOutcome: map[string]int64{}}

	jobs := s.db.WithContext(ctx).Model(&Job{})
	if appID != "" {
		jobs = jobs.Where("app_id = ?", appID)
	}
	var jrows []struct {
		State JobState
		N     int64
	}
	if err := jobs.Select("state, count(*) as n").Group("state").Scan(&jrows).Error; err != nil {
		return Metrics{}, err
	}
	for _, r := range jrows {
		switch r.State {
		case JobPending:
			m.Jobs.Pending = r.N
		case JobLeased:
			m.Jobs.Leased = r.N
		case JobDone:
			m.Jobs.Done = r.N
		case JobFailed:
			m.Jobs.Failed = r.N
		}
	}

	runs := s.db.WithContext(ctx).Model(&Run{}).Where("started_at >= ?", since)
	if appID != "" {
		runs = runs.Where("app_id = ?", appID)
	}
	var orows []struct {
		Outcome string
		N       int64
	}
	if err := runs.Select("outcome, count(*) as n").Group("outcome").Scan(&orows).Error; err != nil {
		return Metrics{}, err
	}
	for _, r := range orows {
		m.ByOutcome[r.Outcome] = r.N
		m.Runs += r.N
		switch r.Outcome {
		case "ok", "pushed":
			m.OK += r.N
		case "failed":
			m.Failed += r.N
		}
	}
	if denom := m.OK + m.Failed; denom > 0 {
		m.SuccessRate = float64(m.OK) / float64(denom)
	}

	durs := s.db.WithContext(ctx).Model(&Run{}).
		Where("started_at >= ? AND duration_ms > 0", since)
	if appID != "" {
		durs = durs.Where("app_id = ?", appID)
	}
	var ds []int64
	if err := durs.Order("started_at desc").Limit(metricsRunCap).Pluck("duration_ms", &ds).Error; err != nil {
		return Metrics{}, err
	}
	m.DurationMs = percentiles(ds)
	return m, nil
}

// percentiles returns p50/p95/max of an unsorted duration slice (0s when empty).
func percentiles(ds []int64) Percentiles {
	if len(ds) == 0 {
		return Percentiles{}
	}
	sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
	return Percentiles{
		P50: ds[pctIndex(len(ds), 50)],
		P95: ds[pctIndex(len(ds), 95)],
		Max: ds[len(ds)-1],
	}
}

func pctIndex(n, p int) int {
	if n == 0 {
		return 0
	}
	i := (p * n) / 100
	if i >= n {
		i = n - 1
	}
	return i
}

// DeadLetter returns terminally-failed jobs (retries exhausted / non-retryable) —
// the DLQ an operator inspects and can replay. Newest activity first.
func (s *Store) DeadLetter(ctx context.Context, appID string, limit int) ([]Job, error) {
	return s.ListJobs(ctx, JobFilter{AppID: appID, State: JobFailed, Limit: limit})
}

// TriggerAlert flags an armed trigger whose most recent runs are all failing —
// the signal that a channel is broken (bad credentials, dead endpoint) and needs
// attention, not a one-off blip.
type TriggerAlert struct {
	TriggerID  string     `json:"trigger_id"`
	AppID      string     `json:"app_id"`
	Provider   string     `json:"provider"`
	Adapter    string     `json:"adapter"`
	FailStreak int        `json:"fail_streak"`
	LastError  string     `json:"last_error,omitempty"`
	LastRun    *time.Time `json:"last_run,omitempty"`
}

// alertScanRuns is how many recent runs per trigger we inspect for a fail streak.
const alertScanRuns = 20

// TriggerAlerts returns one alert per enabled trigger whose latest run failed and
// whose leading (most-recent-first) failure streak is >= minStreak. A single
// success anywhere in the recent window clears the streak. Small N of triggers,
// so a per-trigger recent-run read is fine.
func (s *Store) TriggerAlerts(ctx context.Context, minStreak int) ([]TriggerAlert, error) {
	if minStreak < 1 {
		minStreak = 1
	}
	trigs, err := s.AllTriggers(ctx, "", true)
	if err != nil {
		return nil, err
	}
	var out []TriggerAlert
	for _, t := range trigs {
		runs, err := s.ListRuns(ctx, RunFilter{TriggerID: t.ID, Limit: alertScanRuns})
		if err != nil {
			return nil, err
		}
		streak := 0
		var lastErr string
		var lastRun *time.Time
		for _, r := range runs {
			if r.Outcome != "failed" {
				break
			}
			if streak == 0 {
				lastErr = r.Error
				rt := r.StartedAt
				lastRun = &rt
			}
			streak++
		}
		if streak >= minStreak {
			out = append(out, TriggerAlert{
				TriggerID: t.ID, AppID: t.AppID, Provider: t.Provider, Adapter: t.Adapter,
				FailStreak: streak, LastError: lastErr, LastRun: lastRun,
			})
		}
	}
	return out, nil
}
