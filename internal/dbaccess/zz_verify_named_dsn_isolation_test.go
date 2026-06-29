package dbaccess

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

type dsnDB struct {
	dsn    string
	closed atomic.Bool
}

func (d *dsnDB) Kind() string { return "dsnfp" }
func (d *dsnDB) Query(ctx context.Context, q string, a ...any) (*Result, error) {
	return &Result{Rows: []Row{{"dsn": d.dsn}}, RowCount: 1}, nil
}
func (d *dsnDB) Schema(ctx context.Context) (*Catalog, error) { return &Catalog{}, nil }
func (d *dsnDB) Close() error                                 { d.closed.Store(true); return nil }

// TestVerify_NamedPoolIsolatesByDSN guards the per-user BYOK invariant: two
// callers using the SAME connection name but DIFFERENT DSNs must get DIFFERENT
// pooled connections (never share a socket → never read each other's database).
// Same name + same DSN must still pool to one handle.
func TestVerify_NamedPoolIsolatesByDSN(t *testing.T) {
	var opens atomic.Int64
	Register("dsnfp", func(ctx context.Context, cfg ConnConfig) (DB, error) {
		opens.Add(1)
		return &dsnDB{dsn: cfg.DSN}, nil
	})

	mgr := NewManager(256, 30*time.Minute)
	defer mgr.Shutdown()
	ctx := context.Background()

	cfgA := ConnConfig{Name: "prod", Kind: "dsnfp", DSN: "dsn://userA"}
	cfgB := ConnConfig{Name: "prod", Kind: "dsnfp", DSN: "dsn://userB"}

	dbA, err := mgr.Named(ctx, "app", cfgA)
	if err != nil {
		t.Fatalf("open A: %v", err)
	}
	dbB, err := mgr.Named(ctx, "app", cfgB)
	if err != nil {
		t.Fatalf("open B: %v", err)
	}
	if dbA == dbB {
		t.Fatal("LEAK: same name + different DSN shared one pooled connection")
	}
	if dbA.(*dsnDB).dsn != "dsn://userA" || dbB.(*dsnDB).dsn != "dsn://userB" {
		t.Fatalf("wrong handles: A=%q B=%q", dbA.(*dsnDB).dsn, dbB.(*dsnDB).dsn)
	}

	// Same name + same DSN → reused (no second open).
	dbA2, _ := mgr.Named(ctx, "app", cfgA)
	if dbA2 != dbA {
		t.Fatal("same name + same DSN was not pooled (reopened)")
	}
	if opens.Load() != 2 {
		t.Fatalf("expected exactly 2 distinct opens, got %d", opens.Load())
	}

	// disconnect-by-name still resolves a fingerprinted entry.
	if err := mgr.Close("app", "prod"); err != nil {
		t.Fatalf("close by name: %v", err)
	}
}
