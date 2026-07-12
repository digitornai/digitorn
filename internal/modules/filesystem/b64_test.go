package filesystem

import (
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"
)

// content_b64 lets a large / JSON-heavy body travel escape-safe: base64 has no
// quotes/backslashes/newlines, so it can't break argument encoding.
func TestWrite_ContentB64_Decodes(t *testing.T) {
	m, ctx := hardenModule(t)
	raw := `{"a":"b","n":1,"s":"line1\nline2"}`
	b64 := base64.StdEncoding.EncodeToString([]byte(raw))
	r, err := m.write(ctx, mustJSON(map[string]any{"path": "s.json", "content_b64": b64}))
	if err != nil || !r.Success {
		t.Fatalf("write content_b64: %v %v", err, r.Error)
	}
	rr, _ := m.read(ctx, mustJSON(map[string]any{"path": "s.json"}))
	if !strings.Contains(fmt.Sprint(rr.Data), `{"a":"b","n":1`) {
		t.Fatalf("content_b64 not decoded into file: %v", rr.Data)
	}
}

func TestWrite_ContentB64_InvalidRejected(t *testing.T) {
	m, ctx := hardenModule(t)
	r, _ := m.write(ctx, mustJSON(map[string]any{"path": "x.txt", "content_b64": "!!! not base64 !!!"}))
	if r.Success {
		t.Fatalf("invalid content_b64 must be rejected, not written")
	}
}

// An ambiguous old_string plus the start_line the agent read edits the match
// nearest that line instead of erroring — the line is only a tiebreak between
// identical matches.
func TestEdit_AmbiguousDisambiguatedByLine(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "d.txt", "content": "a\nDUP\nc\nDUP\ne\n"}))
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "d.txt", "old_string": "DUP", "new_string": "X", "start_line": 4}))
	if err != nil || !r.Success {
		t.Fatalf("line-disambiguated edit: %v %v", err, r.Error)
	}
	got := fmt.Sprint(rrData(m, ctx, "d.txt"))
	if strings.Count(got, "DUP") != 1 || !strings.Contains(got, "2\tDUP") {
		t.Fatalf("expected the line-4 DUP replaced, line-2 kept: %v", got)
	}
}

func rrData(m *Module, ctx context.Context, path string) any {
	r, _ := m.read(ctx, mustJSON(map[string]any{"path": path}))
	return r.Data
}

func TestEdit_NewStringB64_Decodes(t *testing.T) {
	m, ctx := hardenModule(t)
	m.write(ctx, mustJSON(map[string]any{"path": "e.txt", "content": "OLD\n"}))
	repl := base64.StdEncoding.EncodeToString([]byte(`NEW-{"x":1}`))
	r, err := m.edit(ctx, mustJSON(map[string]any{"path": "e.txt", "old_string": "OLD", "new_string_b64": repl}))
	if err != nil || !r.Success {
		t.Fatalf("edit new_string_b64: %v %v", err, r.Error)
	}
	rr, _ := m.read(ctx, mustJSON(map[string]any{"path": "e.txt"}))
	if !strings.Contains(fmt.Sprint(rr.Data), `NEW-{"x":1}`) {
		t.Fatalf("new_string_b64 not applied: %v", rr.Data)
	}
}
