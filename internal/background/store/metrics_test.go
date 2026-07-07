package store

import (
	"context"
	"testing"
	"time"
)

func recordRun(t *testing.T, s *Store, trigger, outcome string, dur int64, at time.Time) {
	t.Helper()
	err := s.RecordRun(context.Background(), Run{
		JobID: "j-" + outcome, AppID: "app", TriggerID: trigger, Provider: "p",
		Outcome: outcome, DurationMs: dur, StartedAt: at,
	})
	if err != nil {
		t.Fatalf("record run: %v", err)
	}
}

func TestMetricsWindow(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	recordRun(t, s, "trg1", "ok", 100, now.Add(-1*time.Minute))
	recordRun(t, s, "trg1", "ok", 300, now.Add(-2*time.Minute))
	recordRun(t, s, "trg1", "failed", 50, now.Add(-3*time.Minute))
	recordRun(t, s, "trg1", "ok", 200, now.Add(-48*time.Hour)) // outside 24h window

	m, err := s.MetricsWindow(ctx, "app", now.Add(-24*time.Hour))
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	if m.Runs != 3 {
		t.Errorf("runs = %d, want 3 (the 48h-old one is excluded)", m.Runs)
	}
	if m.OK != 2 || m.Failed != 1 {
		t.Errorf("ok=%d failed=%d, want 2/1", m.OK, m.Failed)
	}
	if m.SuccessRate < 0.66 || m.SuccessRate > 0.67 {
		t.Errorf("success_rate = %v, want ~0.666", m.SuccessRate)
	}
	if m.DurationMs.Max != 300 {
		t.Errorf("max duration = %d, want 300", m.DurationMs.Max)
	}
}

func TestDeadLetter(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	j := enq(t, s, "d1")
	if err := s.Fail(ctx, j.ID, "boom", 0); err != nil { // 0 retry → terminal
		t.Fatalf("fail: %v", err)
	}
	enq(t, s, "d2") // stays pending, not in DLQ

	dlq, err := s.DeadLetter(ctx, "app", 0)
	if err != nil {
		t.Fatalf("dlq: %v", err)
	}
	if len(dlq) != 1 || dlq[0].ID != j.ID {
		t.Fatalf("dlq = %+v, want just the failed job", dlq)
	}
	if dlq[0].LastError != "boom" {
		t.Errorf("last_error = %q, want boom", dlq[0].LastError)
	}
}

func TestTriggerAlerts(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	arm := func(id string) {
		if err := s.UpsertTrigger(ctx, &Trigger{ID: id, AppID: "app", Provider: id, Adapter: "cron", Enabled: true}); err != nil {
			t.Fatalf("arm %s: %v", id, err)
		}
	}
	arm("broken")
	arm("healthy")
	arm("recovered")

	recordRun(t, s, "broken", "failed", 10, now.Add(-3*time.Minute))
	recordRun(t, s, "broken", "failed", 10, now.Add(-2*time.Minute))
	recordRun(t, s, "broken", "failed", 10, now.Add(-1*time.Minute))

	recordRun(t, s, "healthy", "ok", 10, now.Add(-1*time.Minute))

	recordRun(t, s, "recovered", "failed", 10, now.Add(-3*time.Minute))
	recordRun(t, s, "recovered", "failed", 10, now.Add(-2*time.Minute))
	recordRun(t, s, "recovered", "ok", 10, now.Add(-1*time.Minute)) // latest is ok → streak cleared

	alerts, err := s.TriggerAlerts(ctx, 3)
	if err != nil {
		t.Fatalf("alerts: %v", err)
	}
	if len(alerts) != 1 || alerts[0].TriggerID != "broken" {
		t.Fatalf("alerts = %+v, want just broken", alerts)
	}
	if alerts[0].FailStreak != 3 {
		t.Errorf("fail_streak = %d, want 3", alerts[0].FailStreak)
	}
}
