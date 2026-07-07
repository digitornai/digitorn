package userauth

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "ua.db")
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := NewStore(db)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

// A refresh-token handoff (Save, empty access token) must NOT wipe a working
// access token. Regression: Upsert always wrote access_token, so every trigger
// re-push blanked the provisioned token and the next turn 401'd.
func TestSaveDoesNotBlankAccessToken(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)

	if err := s.Upsert(ctx, UserToken{UserID: "u1", AccessToken: "AT-good", ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	mgr := NewManager(s, nil)
	if err := mgr.Save(ctx, "u1", "RT-new"); err != nil {
		t.Fatalf("save: %v", err)
	}

	row, ok := s.Get(ctx, "u1")
	if !ok {
		t.Fatal("row gone after Save")
	}
	if row.AccessToken != "AT-good" {
		t.Errorf("access token = %q, want AT-good (Save must preserve it)", row.AccessToken)
	}
	if row.RefreshToken != "RT-new" {
		t.Errorf("refresh token = %q, want RT-new", row.RefreshToken)
	}
}

// A real refresh (access token present) DOES update it.
func TestUpsertWithAccessTokenUpdatesIt(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	if err := s.Upsert(ctx, UserToken{UserID: "u1", AccessToken: "AT-1", RefreshToken: "RT-1", ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.Upsert(ctx, UserToken{UserID: "u1", AccessToken: "AT-2", ExpiresAt: time.Now().Add(24 * time.Hour)}); err != nil {
		t.Fatalf("update: %v", err)
	}
	row, _ := s.Get(ctx, "u1")
	if row.AccessToken != "AT-2" {
		t.Errorf("access token = %q, want AT-2", row.AccessToken)
	}
	if row.RefreshToken != "RT-1" {
		t.Errorf("refresh token = %q, want RT-1 preserved", row.RefreshToken)
	}
}
