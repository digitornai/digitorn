package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Store is the durable job/trigger core. It takes an already-open *gorm.DB
// (SQLite or Postgres) by injection — no connection logic here — so it stays
// pure and trivially testable, and so the background binary owns the DSN choice.
type Store struct{ db *gorm.DB }

// New wraps a GORM handle. Call Migrate once before use.
func New(db *gorm.DB) *Store { return &Store{db: db} }

// Migrate creates/updates the background tables. Idempotent.
func (s *Store) Migrate() error { return s.db.AutoMigrate(&Trigger{}, &Job{}) }

// NewJob is an inbound event to record durably.
type NewJob struct {
	AppID     string
	TriggerID string
	Provider  string
	DedupKey  string // stable per delivery; drives idempotent intake
	Payload   []byte
}

// Enqueue durably records an event as a pending job, claimable immediately.
// Idempotent by DedupKey: a re-delivery returns (existingJob, created=false)
// with NO second row — the intake-before-ACK + dedup guarantee. Concurrency-safe
// via an INSERT … ON CONFLICT DO NOTHING (SQLite + Postgres).
func (s *Store) Enqueue(ctx context.Context, n NewJob) (Job, bool, error) {
	job := Job{
		ID:          uuid.NewString(),
		AppID:       n.AppID,
		TriggerID:   n.TriggerID,
		Provider:    n.Provider,
		DedupKey:    n.DedupKey,
		PayloadJSON: string(n.Payload),
		State:       JobPending,
		RunAfter:    time.Now().UTC(),
	}
	res := s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "dedup_key"}}, DoNothing: true}).
		Create(&job)
	if res.Error != nil {
		return Job{}, false, res.Error
	}
	if res.RowsAffected == 1 {
		return job, true, nil
	}
	// Conflict: the event already exists — return the original, unchanged.
	var existing Job
	if err := s.db.WithContext(ctx).Where("dedup_key = ?", n.DedupKey).First(&existing).Error; err != nil {
		return Job{}, false, err
	}
	return existing, false, nil
}

// claimablePredicate matches a job that may be (re)claimed right now: pending,
// or a leased job whose lease has expired (the crash-recovery path), and whose
// backoff window has passed.
const claimablePredicate = "(state = ? OR (state = ? AND locked_until < ?)) AND run_after <= ?"

func claimableArgs(now time.Time) []any { return []any{JobPending, JobLeased, now, now} }

// Claim atomically leases up to max claimable jobs (oldest first), bumping
// Attempts and setting a fresh lease (now+leaseTTL). Returns the leased jobs.
// Atomicity:
//   - Postgres: the SELECT takes FOR UPDATE SKIP LOCKED, so concurrent claimers
//     never pick the same row.
//   - SQLite: writes serialize on the single writer; the UPDATE re-checks the
//     claimable predicate and stamps a per-batch ClaimToken, so even if two
//     readers see the same ids the second's UPDATE no-ops on already-leased rows
//     and each claimer reads back exactly what it leased.
//
// Expired leases are claimable here, so this doubles as crash recovery — a job
// whose worker died is re-run once its lease lapses, with no separate sweep.
func (s *Store) Claim(ctx context.Context, max int, leaseTTL time.Duration) ([]Job, error) {
	if max <= 0 {
		return nil, nil
	}
	now := time.Now().UTC()
	until := now.Add(leaseTTL)
	token := uuid.NewString()

	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []string
		sel := tx.Model(&Job{}).
			Where(claimablePredicate, claimableArgs(now)...).
			Order("created_at").Limit(max)
		if tx.Dialector.Name() == "postgres" {
			sel = sel.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := sel.Pluck("id", &ids).Error; err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}
		return tx.Model(&Job{}).
			Where("id IN ?", ids).
			Where(claimablePredicate, claimableArgs(now)...). // re-check: guards the SQLite read-race
			Updates(map[string]any{
				"state":        JobLeased,
				"locked_until": until,
				"claim_token":  token,
				"attempts":     gorm.Expr("attempts + 1"),
			}).Error
	})
	if err != nil {
		return nil, err
	}

	var leased []Job
	if err := s.db.WithContext(ctx).
		Where("claim_token = ?", token).Order("created_at").Find(&leased).Error; err != nil {
		return nil, err
	}
	return leased, nil
}

// Complete marks a leased job done and releases its lease.
func (s *Store) Complete(ctx context.Context, id string) error {
	return s.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).
		Updates(map[string]any{
			"state":        JobDone,
			"locked_until": nil,
			"claim_token":  "",
			"last_error":   "",
		}).Error
}

// Fail releases the lease and either schedules a retry (retryIn > 0 → back to
// pending, not claimable until now+retryIn) or marks the job terminally failed.
func (s *Store) Fail(ctx context.Context, id, reason string, retryIn time.Duration) error {
	upd := map[string]any{
		"last_error":   reason,
		"locked_until": nil,
		"claim_token":  "",
	}
	if retryIn > 0 {
		upd["state"] = JobPending
		upd["run_after"] = time.Now().UTC().Add(retryIn)
	} else {
		upd["state"] = JobFailed
	}
	return s.db.WithContext(ctx).Model(&Job{}).Where("id = ?", id).Updates(upd).Error
}

// ExtendLease pushes a held lease further out — for a long-running job that is
// still making progress, so it isn't reclaimed mid-flight.
func (s *Store) ExtendLease(ctx context.Context, id string, ttl time.Duration) error {
	return s.db.WithContext(ctx).Model(&Job{}).
		Where("id = ? AND state = ?", id, JobLeased).
		Update("locked_until", time.Now().UTC().Add(ttl)).Error
}

// Get returns one job by id (for tests / introspection).
func (s *Store) Get(ctx context.Context, id string) (Job, error) {
	var j Job
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&j).Error
	return j, err
}

// Counts is the job count per state — the /stats observability snapshot.
type Counts struct {
	Pending int64 `json:"pending"`
	Leased  int64 `json:"leased"`
	Done    int64 `json:"done"`
	Failed  int64 `json:"failed"`
}

// Counts aggregates jobs by state in one grouped query.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	var rows []struct {
		State JobState
		N     int64
	}
	if err := s.db.WithContext(ctx).Model(&Job{}).
		Select("state, count(*) as n").Group("state").Scan(&rows).Error; err != nil {
		return Counts{}, err
	}
	var c Counts
	for _, r := range rows {
		switch r.State {
		case JobPending:
			c.Pending = r.N
		case JobLeased:
			c.Leased = r.N
		case JobDone:
			c.Done = r.N
		case JobFailed:
			c.Failed = r.N
		}
	}
	return c, nil
}

// ── Triggers ────────────────────────────────────────────────────────────────

// UpsertTrigger inserts or fully updates an armed listener.
func (s *Store) UpsertTrigger(ctx context.Context, t *Trigger) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	return s.db.WithContext(ctx).
		Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, UpdateAll: true}).
		Create(t).Error
}

// GetTrigger returns one trigger by id (the processor loads a job's trigger to
// read its activation config).
func (s *Store) GetTrigger(ctx context.Context, id string) (Trigger, error) {
	var t Trigger
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&t).Error
	return t, err
}

// ListTriggers returns enabled triggers; appID == "" returns all apps'.
func (s *Store) ListTriggers(ctx context.Context, appID string) ([]Trigger, error) {
	q := s.db.WithContext(ctx).Where("enabled = ?", true)
	if appID != "" {
		q = q.Where("app_id = ?", appID)
	}
	var out []Trigger
	return out, q.Order("created_at").Find(&out).Error
}

// SetCursor durably advances a poller's cursor (committed with the job so a
// restart resumes exactly where it stopped).
func (s *Store) SetCursor(ctx context.Context, triggerID, cursor string) error {
	return s.db.WithContext(ctx).Model(&Trigger{}).
		Where("id = ?", triggerID).Update("cursor", cursor).Error
}
