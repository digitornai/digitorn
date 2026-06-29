package credentials

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

// fakeSealer is a reversible no-crypto sealer so the store's behaviour can be
// tested without a key file. Round-trip equivalence is all the store relies on.
type fakeSealer struct{}

func (fakeSealer) Seal(p []byte) (string, error) { return base64.StdEncoding.EncodeToString(p), nil }
func (fakeSealer) Open(e string) ([]byte, error) { return base64.StdEncoding.DecodeString(e) }

func newTestStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.UserCredential{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(gdb, fakeSealer{})
}

func TestStore_CreateListMaskAndNoPlaintext(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v, err := s.Create(ctx, "user-A", CreateInput{
		ProviderName: "openai",
		ProviderType: "api_key",
		Label:        "prod",
		Fields:       map[string]string{"api_key": "sk-abcdefghijklmnop"},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.ID == "" || v.Scope != "per_user" || v.Status != "valid" {
		t.Fatalf("unexpected view: %+v", v)
	}
	masked := v.FieldsMasked["api_key"]
	if masked == "" || strings.Contains(masked, "abcdefghij") {
		t.Fatalf("preview leaks plaintext or empty: %q", masked)
	}
	if !strings.HasPrefix(masked, "sk-") || !strings.HasSuffix(masked, "mnop") {
		t.Fatalf("preview not a recognisable mask: %q", masked)
	}

	list, err := s.List(ctx, "user-A")
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v len=%d", err, len(list))
	}
}

func TestStore_OwnershipIsolation(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v, err := s.Create(ctx, "user-A", CreateInput{ProviderName: "github", Fields: map[string]string{"api_key": "ghp_secret_value_123"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// user-B sees nothing and cannot touch user-A's row.
	if list, _ := s.List(ctx, "user-B"); len(list) != 0 {
		t.Fatalf("cross-user list leaked %d rows", len(list))
	}
	if _, err := s.Get(ctx, "user-B", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user Get: want ErrNotFound, got %v", err)
	}
	if _, err := s.Update(ctx, "user-B", v.ID, ptr("x"), nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user Update: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "user-B", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-user Delete: want ErrNotFound, got %v", err)
	}
	// Owner still has it.
	if _, err := s.Get(ctx, "user-A", v.ID); err != nil {
		t.Fatalf("owner Get after cross-user attempts: %v", err)
	}
}

func TestStore_UpdateAndDelete(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	v, _ := s.Create(ctx, "u", CreateInput{ProviderName: "openai", Fields: map[string]string{"api_key": "sk-oldvalue1234567"}})

	upd, err := s.Update(ctx, "u", v.ID, ptr("renamed"), map[string]string{"api_key": "sk-newvalue7654321"})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if upd.Label != "renamed" {
		t.Fatalf("label not updated: %q", upd.Label)
	}
	if !strings.HasSuffix(upd.FieldsMasked["api_key"], "4321") {
		t.Fatalf("fields not re-masked after update: %q", upd.FieldsMasked["api_key"])
	}

	if err := s.Delete(ctx, "u", v.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, "u", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after delete: want ErrNotFound, got %v", err)
	}
	if err := s.Delete(ctx, "u", v.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double delete: want ErrNotFound, got %v", err)
	}
}

func TestStore_DefaultsApplied(t *testing.T) {
	s := newTestStore(t)
	v, err := s.Create(context.Background(), "u", CreateInput{ProviderName: "x", Fields: map[string]string{"k": "value-1234567"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if v.ProviderType != "api_key" || v.Label != "default" {
		t.Fatalf("defaults not applied: type=%q label=%q", v.ProviderType, v.Label)
	}
}

func ptr(s string) *string { return &s }
