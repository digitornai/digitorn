package workdir

import (
	"context"
	"path/filepath"
	"testing"
)

func TestEnforceArgs_RewritesValidAndRejectsEscape(t *testing.T) {
	p := NewPolicy(Options{Root: t.TempDir(), Home: t.TempDir()})

	// Valid relative path is rewritten in place to the enforced absolute path.
	args := map[string]any{"path": "notes/a.txt", "limit": float64(10)}
	if err := EnforceArgs(p, args, "path"); err != nil {
		t.Fatalf("valid path must pass: %v", err)
	}
	want := filepath.Join(p.Root(), "notes", "a.txt")
	if args["path"] != want {
		t.Errorf("arg not rewritten: got %v want %q", args["path"], want)
	}
	if args["limit"] != float64(10) {
		t.Errorf("non-path arg must be untouched, got %v", args["limit"])
	}

	// Escape is rejected.
	esc := map[string]any{"path": "../../etc/passwd"}
	if err := EnforceArgs(p, esc, "path"); err == nil {
		t.Error("escaping path must be rejected")
	}
}

func TestEnforceArgs_SkipsAbsentEmptyNonString(t *testing.T) {
	p := NewPolicy(Options{Root: t.TempDir(), Home: t.TempDir()})
	args := map[string]any{"other": "x", "path": "", "count": 3}
	if err := EnforceArgs(p, args, "path", "missing"); err != nil {
		t.Fatalf("absent/empty path keys must be a no-op: %v", err)
	}
	if args["path"] != "" {
		t.Errorf("empty path must stay empty, got %v", args["path"])
	}
}

func TestEnforceArgs_NoKeysNoArgs(t *testing.T) {
	p := NewPolicy(Options{Root: t.TempDir(), Home: t.TempDir()})
	if err := EnforceArgs(p, nil, "path"); err != nil {
		t.Errorf("nil args must be a no-op: %v", err)
	}
	if err := EnforceArgs(p, map[string]any{"path": "x"}); err != nil {
		t.Errorf("no keys must be a no-op: %v", err)
	}
}

func TestCtxRoundTrip(t *testing.T) {
	if _, ok := PathPolicyFromContext(context.Background()); ok {
		t.Error("bare context must carry no policy")
	}
	if _, ok := PathPolicyFromContext(nil); ok {
		t.Error("nil context must carry no policy")
	}
	p := NewPolicy(Options{Root: t.TempDir(), Home: t.TempDir()})
	ctx := WithPathPolicy(context.Background(), p)
	got, ok := PathPolicyFromContext(ctx)
	if !ok || got.Root() != p.Root() {
		t.Errorf("policy round-trip failed: ok=%v root=%q", ok, got.Root())
	}
}
