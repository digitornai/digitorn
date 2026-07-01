package mcpoauth

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

func newTestStateStore(t *testing.T) (*StateStore, *gorm.DB) {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if sqlDB, derr := gdb.DB(); derr == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	if err := gdb.AutoMigrate(&models.OAuthState{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := NewSealer(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return NewStateStore(gdb, sealer), gdb
}

func TestStateStore_ConcurrentTakeIsSingleUse(t *testing.T) {
	s, _ := newTestStateStore(t)
	ctx := context.Background()
	if err := s.Put(ctx, PendingState{State: "race", UserID: "u", Provider: "google", ServerID: "srv", Verifier: "v"}); err != nil {
		t.Fatal(err)
	}
	const n = 16
	var wins int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if got, err := s.TakeValid(ctx, "race"); err == nil && got != nil {
				atomic.AddInt64(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("single-use violated: %d goroutines consumed the same state", wins)
	}
}

func TestStateStore_PutTakeSingleUse(t *testing.T) {
	s, _ := newTestStateStore(t)
	ctx := context.Background()

	in := PendingState{
		State: "st1", UserID: "u", AppID: "app", Provider: "google",
		ServerID: "srv", Verifier: "the-verifier", Nonce: "n", RedirectURI: "https://cb",
	}
	if err := s.Put(ctx, in); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.TakeValid(ctx, "st1")
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if got == nil {
		t.Fatal("expected state, got nil")
	}
	if got.UserID != "u" || got.ServerID != "srv" || got.Verifier != "the-verifier" || got.RedirectURI != "https://cb" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	// Single-use: second take returns nil.
	again, err := s.TakeValid(ctx, "st1")
	if err != nil {
		t.Fatalf("take2: %v", err)
	}
	if again != nil {
		t.Fatal("state should be consumed (single-use)")
	}
}

func TestStateStore_UnknownState(t *testing.T) {
	s, _ := newTestStateStore(t)
	got, err := s.TakeValid(context.Background(), "nope")
	if err != nil || got != nil {
		t.Fatalf("unknown state should be (nil,nil), got (%v,%v)", got, err)
	}
}

func TestStateStore_ExpiredIsRejectedAndPurged(t *testing.T) {
	s, gdb := newTestStateStore(t)
	ctx := context.Background()

	// Insert a row already expired (bypass Put's TTL).
	past := time.Now().UTC().Add(-time.Hour)
	if err := gdb.Create(&models.OAuthState{
		State: "old", UserID: "u", AppID: "a", Provider: "google", ServerID: "s",
		Verifier: []byte("x"), ExpiresAt: past, CreatedAt: past,
	}).Error; err != nil {
		t.Fatal(err)
	}
	got, err := s.TakeValid(ctx, "old")
	if err != nil || got != nil {
		t.Fatalf("expired state should be (nil,nil), got (%v,%v)", got, err)
	}
	var count int64
	gdb.Model(&models.OAuthState{}).Count(&count)
	if count != 0 {
		t.Fatalf("expired row should be purged, %d remain", count)
	}
}
