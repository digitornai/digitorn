package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

func (s *Store) RecordRun(ctx context.Context, r Run) error {
	if r.ID == "" {
		r.ID = uuid.NewString()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now().UTC()
	}
	return s.db.WithContext(ctx).Create(&r).Error
}

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

type JobFilter struct {
	AppID     string
	TriggerID string
	State     JobState
	Limit     int
	Offset    int
}

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

type RunFilter struct {
	AppID     string
	TriggerID string
	JobID     string
	Outcome   string
	Limit     int
	Offset    int
}

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

func (s *Store) ListSchedules(ctx context.Context, appID string) ([]Trigger, error) {
	q := s.db.WithContext(ctx).Model(&Trigger{}).Where("kind = ?", "schedule")
	if appID != "" {
		q = q.Where("app_id = ?", appID)
	}
	var out []Trigger
	return out, q.Order("created_at desc").Find(&out).Error
}

type TriggerStat struct {
	TriggerID string           `json:"trigger_id"`
	Total     int64            `json:"total"`
	ByOutcome map[string]int64 `json:"by_outcome"`
	LastRun   *time.Time       `json:"last_run,omitempty"`
}

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
