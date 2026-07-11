package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// ── Run recording (execution report) ─────────────────────────────────────────

// RecordRun durably inserts one execution-report row. Best-effort: the processor
// calls it AFTER a job attempt, off the durable hot path, so a failed insert is
// logged and ignored (it can never fail the job itself).
func (s *Store) RecordRun(ctx context.Context, r Run) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Create(&r).Error
}

// ── Listing / filtering (observability) ──────────────────────────────────────

// clampPage normalises a page request: limit ∈ [1,500] (default 50), offset ≥ 0.
func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// JobFilter narrows a job-history query. Empty fields are wildcards.
type JobFilter struct {
	AppID     string
	TriggerID string
	State     JobState
	Limit     int
	Offset    int
}

// ListJobs returns matching jobs, newest activity first (UpdatedAt desc), paged.
func (s *Store) ListJobs(ctx context.Context, f JobFilter) ([]Job, error) {
	limit, offset := clampPage(f.Limit, f.Offset)
	q := s.db.WithContext(ctx).Model(&Job{})
	if f.AppID != "" {
		q = q.Where("app_id = ?", f.AppID)
	}
	if f.TriggerID != "" {
		q = q.Where("trigger_id = ?", f.TriggerID)
	}
	if f.State != "" {
		q = q.Where("state = ?", f.State)
	}
	var out []Job
	return out, q.Order("updated_at desc").Limit(limit).Offset(offset).Find(&out).Error
}

// RunFilter narrows an execution-report query. Empty fields are wildcards.
type RunFilter struct {
	AppID     string
	TriggerID string
	JobID     string
	Outcome   string
	Limit     int
	Offset    int
}

// ListRuns returns matching execution reports, newest first (StartedAt desc), paged.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, error) {
	limit, offset := clampPage(f.Limit, f.Offset)
	q := s.db.WithContext(ctx).Model(&Run{})
	if f.AppID != "" {
		q = q.Where("app_id = ?", f.AppID)
	}
	if f.TriggerID != "" {
		q = q.Where("trigger_id = ?", f.TriggerID)
	}
	if f.JobID != "" {
		q = q.Where("job_id = ?", f.JobID)
	}
	if f.Outcome != "" {
		q = q.Where("outcome = ?", f.Outcome)
	}
	var out []Run
	return out, q.Order("started_at desc").Limit(limit).Offset(offset).Find(&out).Error
}

// AllTriggers lists triggers for the ops surface. appID == "" → all apps;
// enabledOnly == false → include disabled ones (so an operator sees the full set,
// not only the live ones). Newest first.
func (s *Store) AllTriggers(ctx context.Context, appID string, enabledOnly bool) ([]Trigger, error) {
	q := s.db.WithContext(ctx).Model(&Trigger{})
	if appID != "" {
		q = q.Where("app_id = ?", appID)
	}
	if enabledOnly {
		q = q.Where("enabled = ?", true)
	}
	var out []Trigger
	return out, q.Order("created_at desc").Find(&out).Error
}

// PurgeApp deletes every trigger, job and run belonging to an app — called when
// the app is uninstalled so no armed listener keeps firing and no run history
// lingers orphaned. Live adapters must be disarmed by the caller BEFORE this
// (the store is pure persistence and holds no adapter handles). Runs in one
// transaction; returns how many triggers, jobs and runs were removed.
func (s *Store) PurgeApp(ctx context.Context, appID string) (triggers, jobs, runs int64, err error) {
	if appID == "" {
		return 0, 0, 0, nil
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		res := tx.Where("app_id = ?", appID).Delete(&Run{})
		if res.Error != nil {
			return res.Error
		}
		runs = res.RowsAffected
		res = tx.Where("app_id = ?", appID).Delete(&Job{})
		if res.Error != nil {
			return res.Error
		}
		jobs = res.RowsAffected
		res = tx.Where("app_id = ?", appID).Delete(&Trigger{})
		if res.Error != nil {
			return res.Error
		}
		triggers = res.RowsAffected
		return nil
	})
	return triggers, jobs, runs, err
}

// ListSchedules returns the user-programmed session wake-ups (Kind="schedule"),
// optionally narrowed to an app. Newest first.
func (s *Store) ListSchedules(ctx context.Context, appID string) ([]Trigger, error) {
	q := s.db.WithContext(ctx).Model(&Trigger{}).Where("kind = ?", "schedule")
	if appID != "" {
		q = q.Where("app_id = ?", appID)
	}
	var out []Trigger
	return out, q.Order("created_at desc").Find(&out).Error
}

// TriggerStat is a per-trigger execution summary for the ops report.
type TriggerStat struct {
	TriggerID string           `json:"trigger_id"`
	Total     int64            `json:"total"`
	ByOutcome map[string]int64 `json:"by_outcome"`
	LastRun   *time.Time       `json:"last_run,omitempty"`
}

// TriggerStats aggregates a trigger's run outcomes + last run time. The counts
// come from a GROUP BY; the last-run time is read via the typed model (a SQL
// max() over a datetime column comes back as text and won't scan into time.Time
// on SQLite, so we read the latest row instead).
func (s *Store) TriggerStats(ctx context.Context, triggerID string) (TriggerStat, error) {
	var rows []struct {
		Outcome string
		N       int64
	}
	if err := s.db.WithContext(ctx).Model(&Run{}).
		Select("outcome, count(*) as n").
		Where("trigger_id = ?", triggerID).
		Group("outcome").Scan(&rows).Error; err != nil {
		return TriggerStat{}, err
	}
	st := TriggerStat{TriggerID: triggerID, ByOutcome: map[string]int64{}}
	for _, r := range rows {
		st.Total += r.N
		st.ByOutcome[r.Outcome] = r.N
	}
	var last []Run
	if err := s.db.WithContext(ctx).Where("trigger_id = ?", triggerID).
		Order("started_at desc").Limit(1).Find(&last).Error; err != nil {
		return TriggerStat{}, err
	}
	if len(last) == 1 {
		t := last[0].StartedAt
		st.LastRun = &t
	}
	return st, nil
}

// ── Control (piloting) ───────────────────────────────────────────────────────

// SetTriggerEnabled flips a trigger's live enabled state. This is a RUNTIME
// override: on restart, config discovery re-arms from YAML and is authoritative,
// so a runtime disable does not survive a restart unless the YAML also changes.
// Returns false if no such trigger exists.
func (s *Store) SetTriggerEnabled(ctx context.Context, id string, enabled bool) (bool, error) {
	res := s.db.WithContext(ctx).Model(&Trigger{}).Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}

// ReplayJob re-queues a finished (done/failed) job so the pool runs it again —
// the operator "retry" control. A pending/leased job is in flight and is left
// untouched. Returns false if the job is missing or not in a replayable state.
func (s *Store) ReplayJob(ctx context.Context, id string) (bool, error) {
	res := s.db.WithContext(ctx).Model(&Job{}).
		Where("id = ? AND state IN ?", id, []JobState{JobDone, JobFailed}).
		Updates(map[string]any{
			"state":        JobPending,
			"run_after":    time.Now().UTC(),
			"locked_until": nil,
			"claim_token":  "",
			"last_error":   "",
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected == 1, nil
}
