package indexer

import (
	"context"
	"os"
	"testing"
	"time"
)

func pgTestDSN() string {
	if d := os.Getenv("INDEXER_PG_DSN"); d != "" {
		return d
	}
	return "postgres://postgres:postgres@localhost:5433/postgres"
}

func TestPgStore_Live(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()

	a, err := NewPgStore(ctx, pgTestDSN())
	if err != nil {
		t.Skipf("no Postgres at %s: %v", pgTestDSN(), err)
	}
	defer a.Close()
	b, err := NewPgStore(ctx, pgTestDSN())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()

	key := "live\x00web\x00site"
	if err := a.Save(key, []byte("0/16ABCD")); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := b.Load(key)
	if err != nil || string(got) != "0/16ABCD" {
		t.Fatalf("shared load = %q err=%v, want 0/16ABCD", got, err)
	}

	relA, okA := a.Acquire(ctx, key)
	if !okA {
		t.Fatal("instance A failed to acquire a free lease")
	}
	if _, okB := b.Acquire(ctx, key); okB {
		t.Fatal("lease not exclusive: B acquired while A holds it (double-index risk)")
	}
	relA()
	relB, okB2 := b.Acquire(ctx, key)
	if !okB2 {
		t.Fatal("B failed to acquire after A released")
	}
	relB()
}

func TestService_PerAppCursor_Live(t *testing.T) {
	registerLoad()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	probe, err := NewPgStore(ctx, pgTestDSN())
	if err != nil {
		t.Skipf("no Postgres: %v", err)
	}
	probe.Close()

	svc := NewService(NewMemCursor(), 4)
	sink := &countSink{}
	spec := SourceSpec{
		Name: "pa", Type: "loadfake", KB: "kb", Owner: "tenantX",
		CursorDSN: pgTestDSN(), Opts: map[string]any{"docs": 3},
	}
	if _, err := svc.Sync(ctx, spec, sink); err != nil {
		t.Fatal(err)
	}

	st, _ := NewPgStore(ctx, pgTestDSN())
	defer st.Close()
	if b, _ := st.Load(stateKey(spec)); len(b) == 0 {
		t.Fatal("sync state did not land in the app's own DB")
	}
	if b, _ := svc.cursor.Load(stateKey(spec)); len(b) != 0 {
		t.Fatal("sync state leaked into the local default cursor")
	}
}

func TestStateKey_PerOwnerIsolation(t *testing.T) {
	a := SourceSpec{Name: "s", Type: "web", KB: "kb", Owner: "app-A"}
	b := SourceSpec{Name: "s", Type: "web", KB: "kb", Owner: "app-B"}
	if stateKey(a) == stateKey(b) {
		t.Fatalf("two apps collide on the same state key: %q", stateKey(a))
	}
}
