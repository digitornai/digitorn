package usersnippets

import (
	"context"
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/persistence/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := gdb.AutoMigrate(&models.UserSnippet{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(gdb)
}

func TestStore_CreateListTagsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sn, err := s.Create(ctx, "user-A", "app-1", "  Greeting  ", "Hello there", "👋", []string{"hi", "intro"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sn.Title != "Greeting" { // trimmed
		t.Fatalf("title=%q", sn.Title)
	}
	if sn.Emoji != "👋" || len(sn.Tags) != 2 {
		t.Fatalf("emoji/tags not stored: %+v", sn)
	}
	if sn.ID == "" || sn.CreatedAt.IsZero() {
		t.Fatalf("missing id/timestamps: %+v", sn)
	}

	list, err := s.List(ctx, "user-A", "app-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}
	// Tags survive the JSON round-trip through the DB.
	if len(list[0].Tags) != 2 || list[0].Tags[0] != "hi" {
		t.Fatalf("tags round-trip failed: %+v", list[0].Tags)
	}
}

func TestStore_RequiredFields(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "user-A", "app-1", "  ", "body", "", nil); err == nil {
		t.Fatal("empty title should error")
	}
	if _, err := s.Create(ctx, "user-A", "app-1", "t", "  ", "", nil); err == nil {
		t.Fatal("empty body should error")
	}
}

func TestStore_Isolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "user-A", "app-1", "a", "x", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "user-B", "app-1", "b", "y", "", nil); err != nil {
		t.Fatal(err)
	}
	a, _ := s.List(ctx, "user-A", "app-1")
	if len(a) != 1 {
		t.Fatalf("user-A list=%d want 1", len(a))
	}
	other, _ := s.List(ctx, "user-A", "app-2")
	if len(other) != 0 {
		t.Fatalf("user-A app-2 list=%d want 0", len(other))
	}
}

func TestStore_Update_Partial(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, _ := s.Create(ctx, "user-A", "app-1", "orig", "origbody", "🙂", []string{"keep"})

	newBody := "newbody"
	upd, err := s.Update(ctx, "user-A", "app-1", sn.ID, nil, &newBody, nil, nil)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	// Only body changed; title/emoji/tags untouched.
	if upd.Body != "newbody" || upd.Title != "orig" || upd.Emoji != "🙂" || len(upd.Tags) != 1 {
		t.Fatalf("partial update wrong: %+v", upd)
	}

	// Clearing tags explicitly (pointer to empty slice).
	empty := []string{}
	upd2, _ := s.Update(ctx, "user-A", "app-1", sn.ID, nil, nil, nil, &empty)
	if len(upd2.Tags) != 0 {
		t.Fatalf("tags not cleared: %+v", upd2.Tags)
	}

	// Cross-user update → not found.
	if _, err := s.Update(ctx, "user-B", "app-1", sn.ID, &newBody, nil, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user update err=%v want ErrNotFound", err)
	}
}

func TestStore_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sn, _ := s.Create(ctx, "user-A", "app-1", "gone", "x", "", nil)

	if err := s.Delete(ctx, "user-B", "app-1", sn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete err=%v want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "user-A", "app-1", sn.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, "user-A", "app-1", sn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err=%v want ErrNotFound", err)
	}
}
