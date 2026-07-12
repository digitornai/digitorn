package filesystem

import (
	"encoding/json"
	"strings"
	"testing"
)

// multi_edit tolerates the `edits` array double-encoded as a JSON string.
func TestMultiEdit_StringifiedEditsArray(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "f.txt", "content": "A\nB\n"}))
	editsJSON := `[{"old_string":"A","new_string":"X"},{"old_string":"B","new_string":"Y"}]`
	raw, _ := json.Marshal(map[string]any{"path": "f.txt", "edits": editsJSON}) // edits as STRING
	r, err := m.multiEdit(ctx, raw)
	if err != nil || !r.Success {
		t.Fatalf("stringified edits: err=%v result=%v", err, r.Error)
	}
	rr, _ := m.read(ctx, mustJSON(map[string]any{"path": "f.txt"}))
	got := strings.ReplaceAll(strings.ReplaceAll(rr.Data.(string), "1\t", ""), "2\t", "")
	if !strings.Contains(got, "X") || !strings.Contains(got, "Y") {
		t.Fatalf("edits not applied from stringified array: %v", rr.Data)
	}
}
