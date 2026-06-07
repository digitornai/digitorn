package userskills

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := gdb.AutoMigrate(&models.UserSkill{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(gdb)
}

func TestStore_CreateListGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	sk, err := s.Create(ctx, "user-A", "app-1", "Commit", "Make a commit", "Run git commit.")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if sk.Name != "commit" { // normalized to lowercase
		t.Fatalf("name=%q want commit", sk.Name)
	}
	if sk.ID == "" || sk.CreatedAt.IsZero() {
		t.Fatalf("missing id/timestamps: %+v", sk)
	}

	list, err := s.List(ctx, "user-A", "app-1")
	if err != nil || len(list) != 1 {
		t.Fatalf("list=%d err=%v", len(list), err)
	}

	got, found, err := s.GetByName(ctx, "user-A", "app-1", "/commit")
	if err != nil || !found {
		t.Fatalf("getByName found=%v err=%v", found, err)
	}
	_ = got
	if _, found, _ := s.GetByName(ctx, "user-A", "app-1", "nope"); found {
		t.Fatal("unexpected match for missing name")
	}
}

func TestStore_Isolation_PerUserPerApp(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Same name, different users, same app : both allowed, independent.
	if _, err := s.Create(ctx, "user-A", "app-1", "deploy", "", "A deploy"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, "user-B", "app-1", "deploy", "", "B deploy"); err != nil {
		t.Fatalf("user-B same name should be allowed: %v", err)
	}
	// user-A doesn't see user-B's skill.
	a, _ := s.List(ctx, "user-A", "app-1")
	if len(a) != 1 {
		t.Fatalf("user-A list=%d want 1", len(a))
	}
	// user-A on a different app sees nothing.
	other, _ := s.List(ctx, "user-A", "app-2")
	if len(other) != 0 {
		t.Fatalf("user-A app-2 list=%d want 0", len(other))
	}
}

func TestStore_Create_Conflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.Create(ctx, "user-A", "app-1", "build", "", "x"); err != nil {
		t.Fatal(err)
	}
	_, err := s.Create(ctx, "user-A", "app-1", "BUILD", "", "y") // case-folds to same slug
	if !errors.Is(err, ErrNameConflict) {
		t.Fatalf("err=%v want ErrNameConflict", err)
	}
}

func TestStore_Create_InvalidName(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	for _, bad := range []string{"", "-bad", "has space", "UPPER ONLY!", strings.Repeat("a", 65)} {
		if _, err := s.Create(ctx, "user-A", "app-1", bad, "", "x"); !errors.Is(err, ErrInvalidName) {
			t.Fatalf("name %q: err=%v want ErrInvalidName", bad, err)
		}
	}
}

func TestStore_Update(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sk, _ := s.Create(ctx, "user-A", "app-1", "one", "first", "do one")

	newName := "two"
	newInstr := "do two"
	upd, err := s.Update(ctx, "user-A", "app-1", sk.ID, &newName, nil, &newInstr)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Name != "two" || upd.Instructions != "do two" || upd.Description != "first" {
		t.Fatalf("update result: %+v", upd)
	}
	if !upd.UpdatedAt.After(sk.UpdatedAt) && !upd.UpdatedAt.Equal(sk.UpdatedAt) {
		t.Fatalf("updated_at not advanced")
	}

	// Update on someone else's id → not found.
	if _, err := s.Update(ctx, "user-B", "app-1", sk.ID, &newName, nil, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user update err=%v want ErrNotFound", err)
	}
}

func TestStore_Update_NameConflict(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	_, _ = s.Create(ctx, "user-A", "app-1", "alpha", "", "x")
	b, _ := s.Create(ctx, "user-A", "app-1", "beta", "", "y")
	clash := "alpha"
	if _, err := s.Update(ctx, "user-A", "app-1", b.ID, &clash, nil, nil); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("err=%v want ErrNameConflict", err)
	}
}

func TestStore_Delete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	sk, _ := s.Create(ctx, "user-A", "app-1", "gone", "", "x")

	if err := s.Delete(ctx, "user-B", "app-1", sk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user delete err=%v want ErrNotFound", err)
	}
	if err := s.Delete(ctx, "user-A", "app-1", sk.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := s.Delete(ctx, "user-A", "app-1", sk.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete err=%v want ErrNotFound", err)
	}
}
