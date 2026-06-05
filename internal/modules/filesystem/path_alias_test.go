package filesystem

import "testing"

// TestPathAlias_AcceptsFilePath proves the recurrent "path must not be empty"
// bug is fixed: read/write/edit/multi_edit now accept the file under file_path
// (Claude's editor convention), filename, or file — not only the canonical
// `path` key — so a model that keys the file the "wrong" way still lands.
func TestPathAlias_AcceptsFilePath(t *testing.T) {
	m, ctx := hardenModule(t)

	// write via file_path
	if r, err := m.write(ctx, mustJSON(map[string]any{"file_path": "a.txt", "content": "alpha\nbravo\n"})); err != nil || !r.Success {
		t.Fatalf("write via file_path: err=%v error=%q", err, r.Error)
	}
	// read via filename
	if r, err := m.read(ctx, mustJSON(map[string]any{"filename": "a.txt"})); err != nil || !r.Success {
		t.Fatalf("read via filename: err=%v error=%q", err, r.Error)
	}
	// edit via file_path
	if r, err := m.edit(ctx, mustJSON(map[string]any{"file_path": "a.txt", "old_string": "alpha", "new_string": "ALPHA"})); err != nil || !r.Success {
		t.Fatalf("edit via file_path: err=%v error=%q", err, r.Error)
	}
	// multi_edit via file (third alias)
	if r, err := m.multiEdit(ctx, mustJSON(map[string]any{
		"file":  "a.txt",
		"edits": []map[string]any{{"old_string": "bravo", "new_string": "BRAVO"}},
	})); err != nil || !r.Success {
		t.Fatalf("multi_edit via file: err=%v error=%q", err, r.Error)
	}
	// canonical `path` still works (alias must not break it)
	if r, err := m.edit(ctx, mustJSON(map[string]any{"path": "a.txt", "old_string": "ALPHA", "new_string": "alpha"})); err != nil || !r.Success {
		t.Fatalf("edit via path: err=%v error=%q", err, r.Error)
	}
}
