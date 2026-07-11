package store

import (
	"context"
	"testing"
	"time"
)

func TestRecordAndListRuns(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 20, 0, 0, 0, time.UTC)
	mk := func(id, app, trig, outcome string, at time.Time) {
		if err := s.RecordRun(ctx, Run{ID: id, JobID: "j-" + id, AppID: app, TriggerID: trig, Outcome: outcome, StartedAt: at}); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	mk("r1", "app", "t1", "ok", base)
	mk("r2", "app", "t1", "failed", base.Add(time.Minute))
	mk("r3", "app", "t2", "ok", base.Add(2*time.Minute))
	mk("r4", "other", "t9", "ok", base.Add(3*time.Minute))

	// All runs, newest first.
	all, err := s.ListRuns(ctx, RunFilter{})
	if err != nil || len(all) != 4 {
		t.Fatalf("list all: n=%d err=%v", len(all), err)
	}
	if all[0].ID != "r4" || all[3].ID != "r1" {
		t.Fatalf("not newest-first: %s..%s", all[0].ID, all[3].ID)
	}
	// Filter by trigger.
	t1, _ := s.ListRuns(ctx, RunFilter{TriggerID: "t1"})
	if len(t1) != 2 {
		t.Fatalf("trigger filter: %d", len(t1))
	}
	// Filter by outcome.
	failed, _ := s.ListRuns(ctx, RunFilter{Outcome: "failed"})
	if len(failed) != 1 || failed[0].ID != "r2" {
		t.Fatalf("outcome filter: %+v", failed)
	}
	// Filter by app.
	other, _ := s.ListRuns(ctx, RunFilter{AppID: "other"})
	if len(other) != 1 || other[0].ID != "r4" {
		t.Fatalf("app filter: %+v", other)
	}
	// Paging.
	page, _ := s.ListRuns(ctx, RunFilter{Limit: 2, Offset: 1})
	if len(page) != 2 || page[0].ID != "r3" {
		t.Fatalf("paging: %+v", page)
	}
}

func TestPurgeApp(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	// Arm triggers + record jobs/runs for two apps.
	for _, app := range []string{"gone", "keep"} {
		if err := s.UpsertTrigger(ctx, &Trigger{ID: "trig-" + app, AppID: app, Provider: "discord", Adapter: "webhook", Enabled: true}); err != nil {
			t.Fatalf("trigger %s: %v", app, err)
		}
		if _, _, err := s.Enqueue(ctx, NewJob{AppID: app, TriggerID: "trig-" + app, DedupKey: "d-" + app}); err != nil {
			t.Fatalf("enqueue %s: %v", app, err)
		}
		if err := s.RecordRun(ctx, Run{ID: "run-" + app, JobID: "j-" + app, AppID: app, TriggerID: "trig-" + app, Outcome: "ok"}); err != nil {
			t.Fatalf("run %s: %v", app, err)
		}
	}

	trigs, jobs, runs, err := s.PurgeApp(ctx, "gone")
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if trigs != 1 || jobs != 1 || runs != 1 {
		t.Fatalf("purge counts: triggers=%d jobs=%d runs=%d", trigs, jobs, runs)
	}
	// The purged app has nothing left.
	if gt, _ := s.AllTriggers(ctx, "gone", false); len(gt) != 0 {
		t.Errorf("gone triggers survived: %d", len(gt))
	}
	if gr, _ := s.ListRuns(ctx, RunFilter{AppID: "gone"}); len(gr) != 0 {
		t.Errorf("gone runs survived: %d", len(gr))
	}
	if gj, _ := s.ListJobs(ctx, JobFilter{AppID: "gone"}); len(gj) != 0 {
		t.Errorf("gone jobs survived: %d", len(gj))
	}
	// The other app is untouched.
	if kt, _ := s.AllTriggers(ctx, "keep", false); len(kt) != 1 {
		t.Errorf("keep triggers: %d", len(kt))
	}
	if kr, _ := s.ListRuns(ctx, RunFilter{AppID: "keep"}); len(kr) != 1 {
		t.Errorf("keep runs: %d", len(kr))
	}
}

func TestTriggerStats(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	base := time.Date(2026, 6, 11, 20, 0, 0, 0, time.UTC)
	_ = s.RecordRun(ctx, Run{ID: "a", JobID: "1", AppID: "app", TriggerID: "t1", Outcome: "ok", StartedAt: base})
	_ = s.RecordRun(ctx, Run{ID: "b", JobID: "2", AppID: "app", TriggerID: "t1", Outcome: "ok", StartedAt: base.Add(time.Minute)})
	_ = s.RecordRun(ctx, Run{ID: "c", JobID: "3", AppID: "app", TriggerID: "t1", Outcome: "failed", StartedAt: base.Add(2 * time.Minute)})

	st, err := s.TriggerStats(ctx, "t1")
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Total != 3 || st.ByOutcome["ok"] != 2 || st.ByOutcome["failed"] != 1 {
		t.Fatalf("counts wrong: %+v", st)
	}
	if st.LastRun == nil || !st.LastRun.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("last run wrong: %v", st.LastRun)
	}
}

func TestListJobs_Filters(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	enq(t, s, "d1")
	enq(t, s, "d2")
	// One job leased+completed to test state filter.
	j := enq(t, s, "d3")
	if _, err := s.Claim(ctx, 10, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Complete(ctx, j.ID); err != nil {
		t.Fatalf("complete: %v", err)
	}

	all, err := s.ListJobs(ctx, JobFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("list all jobs: n=%d err=%v", len(all), err)
	}
	done, _ := s.ListJobs(ctx, JobFilter{State: JobDone})
	if len(done) != 1 || done[0].ID != j.ID {
		t.Fatalf("state filter: %+v", done)
	}
	// Limit cap is honoured.
	one, _ := s.ListJobs(ctx, JobFilter{Limit: 1})
	if len(one) != 1 {
		t.Fatalf("limit: %d", len(one))
	}
}

func TestAllTriggers_IncludesDisabled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.UpsertTrigger(ctx, &Trigger{ID: "en", AppID: "app", Provider: "p", Adapter: "cron", Enabled: true})
	_ = s.UpsertTrigger(ctx, &Trigger{ID: "dis", AppID: "app", Provider: "p", Adapter: "cron", Enabled: false})

	// Enabled-only (existing behaviour) sees 1.
	en, _ := s.AllTriggers(ctx, "app", true)
	if len(en) != 1 || en[0].ID != "en" {
		t.Fatalf("enabled-only: %+v", en)
	}
	// Ops view sees both.
	both, _ := s.AllTriggers(ctx, "app", false)
	if len(both) != 2 {
		t.Fatalf("ops view should include disabled: %d", len(both))
	}
}

func TestSetTriggerEnabled(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()
	_ = s.UpsertTrigger(ctx, &Trigger{ID: "t1", AppID: "app", Provider: "p", Adapter: "cron", Enabled: true})

	ok, err := s.SetTriggerEnabled(ctx, "t1", false)
	if err != nil || !ok {
		t.Fatalf("disable: ok=%v err=%v", ok, err)
	}
	got, _ := s.GetTrigger(ctx, "t1")
	if got.Enabled {
		t.Fatal("trigger should be disabled")
	}
	// Missing trigger → ok=false, no error.
	ok, err = s.SetTriggerEnabled(ctx, "ghost", true)
	if err != nil || ok {
		t.Fatalf("missing trigger: ok=%v err=%v", ok, err)
	}
}

func TestReplayJob(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	// A failed job is replayable → back to pending.
	j := enq(t, s, "rj1")
	if _, err := s.Claim(ctx, 10, time.Minute); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.Fail(ctx, j.ID, "boom", 0); err != nil { // terminal fail
		t.Fatalf("fail: %v", err)
	}
	ok, err := s.ReplayJob(ctx, j.ID)
	if err != nil || !ok {
		t.Fatalf("replay failed job: ok=%v err=%v", ok, err)
	}
	got, _ := s.Get(ctx, j.ID)
	if got.State != JobPending || got.LastError != "" {
		t.Fatalf("replayed job should be pending+clean: %+v", got)
	}

	// A pending job is in-flight → NOT replayable.
	p := enq(t, s, "rj2")
	ok, _ = s.ReplayJob(ctx, p.ID)
	if ok {
		t.Fatal("a pending job must not be replayable")
	}
	// Missing job → false.
	ok, _ = s.ReplayJob(ctx, "ghost")
	if ok {
		t.Fatal("missing job must not be replayable")
	}
}
