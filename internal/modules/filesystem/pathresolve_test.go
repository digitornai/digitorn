package filesystem

import (
	"fmt"
	"strings"
	"testing"
)

// read tolerates a wrong-case path when the match is unambiguous.
func TestRead_PathToleranceCase(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "src/App.tsx", "content": "export const A = 1\n"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "src/app.tsx"}))
	if err != nil || !r.Success {
		t.Fatalf("case-insensitive read: err=%v result=%v", err, r.Error)
	}
	if !strings.Contains(fmt.Sprint(r.Data), "export const A = 1") {
		t.Fatalf("wrong content resolved: %v", r.Data)
	}
}

// read tolerates a bare basename when exactly one file carries it.
func TestRead_PathToleranceBasename(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "src/components/Button.tsx", "content": "BTN\n"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "Button.tsx"}))
	if err != nil || !r.Success || !strings.Contains(fmt.Sprint(r.Data), "BTN") {
		t.Fatalf("basename read: err=%v result=%v data=%v", err, r.Error, r.Data)
	}
}

// an ambiguous basename never guesses — it lists the candidates.
func TestRead_PathAmbiguousLists(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "a/dup.txt", "content": "1"}))
	m.write(ctx, mustJSON(map[string]any{"path": "b/dup.txt", "content": "2"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "dup.txt"}))
	msg := r.Error
	if err != nil {
		msg += " " + err.Error()
	}
	if r.Success || !strings.Contains(msg, "a/dup.txt") || !strings.Contains(msg, "b/dup.txt") {
		t.Fatalf("ambiguous basename must list candidates, got success=%v msg=%q", r.Success, msg)
	}
}

// a genuinely missing file still gets the honest not-found hint.
func TestRead_MissingStillErrors(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "x.txt", "content": "hi"}))
	r, err := m.read(ctx, mustJSON(map[string]any{"path": "totally-absent.txt"}))
	if r.Success && err == nil {
		t.Fatalf("missing file must error")
	}
}

// edit resolves a bare basename to the single matching file.
func TestEdit_PathToleranceBasename(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "deep/nested/Config.go", "content": "OLD\n"}))
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "Config.go", "old_string": "OLD", "new_string": "NEW"}))
	if err != nil || !r.Success {
		t.Fatalf("basename edit: err=%v result=%v", err, r.Error)
	}
	rr, _ := m.read(ctx, mustJSON(map[string]any{"path": "deep/nested/Config.go"}))
	if !strings.Contains(fmt.Sprint(rr.Data), "NEW") {
		t.Fatalf("edit not applied to resolved path: %v", rr.Data)
	}
}
