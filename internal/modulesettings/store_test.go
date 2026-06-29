package modulesettings

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/persistence/models"
)

type fakeSealer struct{}

func (fakeSealer) Seal(p []byte) (string, error) { return base64.StdEncoding.EncodeToString(p), nil }
func (fakeSealer) Open(e string) ([]byte, error) { return base64.StdEncoding.DecodeString(e) }

func TestDeepMerge(t *testing.T) {
	base := map[string]any{
		"a": 1.0,
		"sec": map[string]any{"mode": "read_only", "max_rows": 100.0},
		"dbs": []any{map[string]any{"name": "x"}},
	}
	overlay := map[string]any{
		"sec": map[string]any{"max_rows": 5.0}, // nested merge
		"dbs": []any{map[string]any{"name": "y"}, map[string]any{"name": "z"}}, // array replace
	}
	out := DeepMerge(base, overlay)
	sec := out["sec"].(map[string]any)
	if sec["mode"] != "read_only" || sec["max_rows"] != 5.0 {
		t.Fatalf("nested merge wrong: %+v", sec)
	}
	if len(out["dbs"].([]any)) != 2 {
		t.Fatalf("array should be replaced wholesale: %+v", out["dbs"])
	}
	// base untouched
	if len(base["dbs"].([]any)) != 1 {
		t.Fatal("base mutated")
	}
}

func TestDiff(t *testing.T) {
	defaults := map[string]any{"mode": "read_only", "max_rows": 100.0, "host": "default"}
	submitted := map[string]any{"mode": "read_only", "max_rows": 5.0, "host": "default"}
	d := Diff(submitted, defaults)
	if len(d) != 1 || d["max_rows"] != 5.0 {
		t.Fatalf("expected only max_rows delta, got %+v", d)
	}
}

func newStore(t *testing.T) *Store {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.UserModuleConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return NewStore(gdb, fakeSealer{})
}

func TestStore_SetDeltasInvalidate(t *testing.T) {
	s := newStore(t)
	ctx := context.Background()

	if d := s.Deltas(ctx, "u", "app", "database"); len(d) != 0 {
		t.Fatal("expected empty deltas")
	}
	deltas := map[string]any{"databases": []any{map[string]any{"name": "prod", "dsn": "postgres://x"}}}
	if err := s.Set(ctx, "u", "app", "database", deltas); err != nil {
		t.Fatalf("set: %v", err)
	}
	got := s.Deltas(ctx, "u", "app", "database")
	b, _ := json.Marshal(got)
	if !contains(string(b), "postgres://x") {
		t.Fatalf("deltas not persisted: %s", b)
	}
	// isolation
	if d := s.Deltas(ctx, "other", "app", "database"); len(d) != 0 {
		t.Fatal("cross-user leak")
	}
	// upsert + invalidate
	if err := s.Set(ctx, "u", "app", "database", map[string]any{"x": 1.0}); err != nil {
		t.Fatalf("set2: %v", err)
	}
	got2 := s.Deltas(ctx, "u", "app", "database")
	if got2["x"] != 1.0 {
		t.Fatalf("cache not invalidated on save: %+v", got2)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
