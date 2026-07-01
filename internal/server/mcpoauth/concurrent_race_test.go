package mcpoauth

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

func TestStateStore_ConcurrentRaceForSameState(t *testing.T) {
	gdb, _ := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if sqlDB, err := gdb.DB(); err == nil {
		sqlDB.SetMaxOpenConns(1)
	}
	gdb.AutoMigrate(&models.OAuthState{})
	sealer, _ := NewSealer(filepath.Join(t.TempDir(), "server.key"))
	s := NewStateStore(gdb, sealer)
	ctx := context.Background()

	// Create a state
	p := PendingState{
		State: "concurrent-state", UserID: "u", AppID: "app",
		Provider: "google", ServerID: "srv", Verifier: "v", Nonce: "n", RedirectURI: "https://cb",
	}
	s.Put(ctx, p)

	var wg sync.WaitGroup
	var successCount int32

	// Two concurrent TakeValid calls for the same state
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, _ := s.TakeValid(ctx, "concurrent-state")
			if got != nil {
				atomic.AddInt32(&successCount, 1)
			}
		}()
	}

	wg.Wait()

	// Only ONE should succeed
	if successCount != 1 {
		t.Fatalf("Expected 1 success, got %d — race condition detected!", successCount)
	}
}
