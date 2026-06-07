// Package store is the durable core of the Digitorn background service: the
// crash-survivable queue of inbound channel events ("jobs") and the registry of
// armed listeners ("triggers"). It is pure persistence — no adapters, no daemon
// coupling — backed by GORM so the SAME code runs on SQLite (local daemon) and
// Postgres (cloud), selected only by the injected *gorm.DB.
//
// Durability contract (the three Python bugs, fixed):
//   - intake-before-ACK: an event becomes a durable Job before the adapter ACKs;
//   - lease-not-memory: a Job is claimed with a lease, so a crash re-runs it
//     instead of losing it;
//   - dedup: a stable DedupKey makes intake idempotent (no double-fire).
package store

import "time"

// JobState is the lifecycle of one durable event.
type JobState string

const (
	JobPending JobState = "pending" // waiting to be claimed
	JobLeased  JobState = "leased"  // claimed by a worker, lease held until LockedUntil
	JobDone    JobState = "done"    // processed successfully
	JobFailed  JobState = "failed"  // terminal failure (retries exhausted / non-retryable)
)

// Trigger is an armed channel listener for an app, persisted so it survives a
// restart and so pollers resume from their Cursor.
type Trigger struct {
	ID         string `gorm:"size:64;primaryKey"`
	AppID      string `gorm:"size:128;index;not null"`
	Provider   string `gorm:"size:128;not null"` // the app's configured channel name
	Adapter    string `gorm:"size:64;not null"`  // webhook | cron | rss | email | …
	ConfigJSON string `gorm:"type:text"`         // adapter config (opaque here)
	Cursor     string `gorm:"type:text"`         // poller cursor (rss guid / imap uid …)
	// Enabled has NO gorm default: a default would make GORM omit a false value
	// on insert (the zero-value gotcha) and silently re-enable a disabled trigger.
	// Callers (config discovery) set it explicitly when arming.
	Enabled    bool `gorm:"not null;index"`
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

func (Trigger) TableName() string { return "bg_triggers" }

// Job is one durable unit of work: an inbound event to turn into an agentic
// session. DedupKey carries a UNIQUE index so re-delivery never creates a
// second job. The (State, RunAfter, LockedUntil) trio drives the claimable
// predicate; ClaimToken tags a claim batch so a claimer reads back exactly the
// rows it leased — correct on SQLite (single-writer) and Postgres (SKIP LOCKED).
type Job struct {
	ID          string     `gorm:"size:64;primaryKey"`
	AppID       string     `gorm:"size:128;index;not null"`
	TriggerID   string     `gorm:"size:64;index"`
	Provider    string     `gorm:"size:128"`
	DedupKey    string     `gorm:"size:256;uniqueIndex;not null"`
	PayloadJSON string     `gorm:"type:text"`
	State       JobState   `gorm:"size:16;index;not null;default:pending"`
	Attempts    int        `gorm:"not null;default:0"`
	RunAfter    time.Time  `gorm:"index"` // not claimable before this (backoff)
	LockedUntil *time.Time // lease expiry; null unless leased
	ClaimToken  string     `gorm:"size:64;index"`
	LastError   string     `gorm:"type:text"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

func (Job) TableName() string { return "bg_jobs" }
