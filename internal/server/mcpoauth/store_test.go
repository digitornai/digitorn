package mcpoauth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.Credential{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := NewSealer(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	return NewStore(gdb, sealer)
}

func TestStore_SetGetDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if tok, err := s.Get(ctx, "user-A", "google"); err != nil || tok != nil {
		t.Fatalf("expected (nil,nil) for missing, got (%v,%v)", tok, err)
	}

	want := &Token{AccessToken: "acc", RefreshToken: "ref", TokenType: "Bearer", ExpiresAt: 123, Scope: "drive"}
	if err := s.Set(ctx, "user-A", "google", want); err != nil {
		t.Fatalf("set: %v", err)
	}
	got, err := s.Get(ctx, "user-A", "google")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil || *got != *want {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}

	// Upsert replaces in place (no duplicate row).
	want2 := &Token{AccessToken: "acc2", RefreshToken: "ref2"}
	if err := s.Set(ctx, "user-A", "google", want2); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	got2, _ := s.Get(ctx, "user-A", "google")
	if got2 == nil || got2.AccessToken != "acc2" {
		t.Fatalf("upsert not applied: %+v", got2)
	}
	var count int64
	s.db.Model(&models.Credential{}).Where("user_id = ? AND provider = ?", "user-A", "google").Count(&count)
	if count != 1 {
		t.Fatalf("expected 1 row after upsert, got %d", count)
	}
}

func TestStore_DeleteIsScoped(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.Set(ctx, "user-A", "google", &Token{AccessToken: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "user-B", "google", &Token{AccessToken: "b"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "user-A", "github", &Token{AccessToken: "g"}); err != nil {
		t.Fatal(err)
	}

	if err := s.Delete(ctx, "user-A", "google"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	if tok, _ := s.Get(ctx, "user-A", "google"); tok != nil {
		t.Fatal("deleted token still present")
	}
	if tok, _ := s.Get(ctx, "user-B", "google"); tok == nil || tok.AccessToken != "b" {
		t.Fatal("other user's token was affected by delete")
	}
	if tok, _ := s.Get(ctx, "user-A", "github"); tok == nil || tok.AccessToken != "g" {
		t.Fatal("same user's other provider was affected by delete")
	}
}
