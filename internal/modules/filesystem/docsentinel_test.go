package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/docstore"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

func seedDoc(t *testing.T, root string) string {
	t.Helper()
	composed := filepath.Join(root, "scene.excalidraw")
	doc := `{"type":"excalidraw","elements":[{"id":"title","type":"text","index":"a0","text":"Hi"}],"files":{}}`
	if err := os.WriteFile(composed, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	m := docstore.Manifest{
		Match: "*.excalidraw", Root: "meta.json",
		Collections: []docstore.Collection{
			{Name: "elements", Path: "/elements", ID: "id", Grain: "item", Order: "field:index"},
			{Name: "files", Path: "/files", ID: "$key", Grain: "item"},
		},
		Validate: docstore.Validate{UniqueID: true, Refs: []docstore.Ref{
			{Field: "startBinding.elementId", In: "elements"},
		}},
	}
	if err := docstore.ExplodeFile(composed, m); err != nil {
		t.Fatal(err)
	}
	return composed
}

func docModule(t *testing.T) (*Module, context.Context, string) {
	t.Helper()
	root := t.TempDir()
	m := New()
	if err := m.Init(context.Background(), map[string]any{"workspace": root}); err != nil {
		t.Fatalf("init: %v", err)
	}
	pp := workdir.NewPolicy(workdir.Options{Root: root, Home: t.TempDir()})
	return m, workdir.WithPathPolicy(context.Background(), pp), root
}

func syncOf(t *testing.T, r any) map[string]any {
	t.Helper()
	data, ok := r.(map[string]any)
	if !ok {
		t.Fatalf("data is %T", r)
	}
	s, ok := data["sync"].(map[string]any)
	if !ok {
		t.Fatalf("no sync payload in result: %v", data)
	}
	return s
}

func TestSentinel_FragmentWriteComposes(t *testing.T) {
	m, ctx, root := docModule(t)
	composed := seedDoc(t, root)
	r, err := m.write(ctx, mustJSON(map[string]any{
		"path":    "scene.excalidraw.d/elements/box.json",
		"content": `{"id":"box","type":"rectangle","index":"a1","x":10}`,
	}))
	if err != nil || !r.Success {
		t.Fatalf("write: %v %v", err, r.Error)
	}
	s := syncOf(t, r.Data)
	if s["composed_ok"] != true {
		t.Fatalf("expected composed_ok=true: %v", s)
	}
	b, _ := os.ReadFile(composed)
	if !strings.Contains(string(b), `"id":"box"`) {
		t.Fatalf("composed not updated: %s", b)
	}
}

func TestSentinel_BrokenFragmentDiagnosed(t *testing.T) {
	m, ctx, root := docModule(t)
	composed := seedDoc(t, root)
	before, _ := os.ReadFile(composed)
	r, err := m.write(ctx, mustJSON(map[string]any{
		"path":    "scene.excalidraw.d/elements/bad.json",
		"content": `{"id":"bad", "x": }`,
	}))
	if err != nil || !r.Success {
		t.Fatalf("write itself should succeed: %v %v", err, r.Error)
	}
	s := syncOf(t, r.Data)
	if s["composed_ok"] != false {
		t.Fatalf("compose must be refused: %v", s)
	}
	diags := fmt.Sprint(s["diagnostics"])
	if !strings.Contains(diags, "bad.json") || !strings.Contains(diags, "at byte") {
		t.Fatalf("diagnostics must name file+offset: %v", diags)
	}
	after, _ := os.ReadFile(composed)
	if string(before) != string(after) {
		t.Fatalf("composed must stay on last-good state")
	}
}

func TestSentinel_DanglingRefHint(t *testing.T) {
	m, ctx, root := docModule(t)
	seedDoc(t, root)
	r, _ := m.write(ctx, mustJSON(map[string]any{
		"path":    "scene.excalidraw.d/elements/arrow.json",
		"content": `{"id":"arrow","type":"arrow","index":"a2","startBinding":{"elementId":"titel"}}`,
	}))
	s := syncOf(t, r.Data)
	diags := fmt.Sprint(s["diagnostics"])
	if !strings.Contains(diags, "closest id") || !strings.Contains(diags, "title") {
		t.Fatalf("want closest-id hint, got %v", diags)
	}
}

func TestSentinel_ComposedWriteDecomposes(t *testing.T) {
	m, ctx, root := docModule(t)
	seedDoc(t, root)
	doc := map[string]any{}
	b, _ := os.ReadFile(filepath.Join(root, "scene.excalidraw"))
	json.Unmarshal(b, &doc)
	doc["elements"].([]any)[0].(map[string]any)["text"] = "Moved"
	edited, _ := json.Marshal(doc)
	r, err := m.write(ctx, mustJSON(map[string]any{
		"path": "scene.excalidraw", "content": string(edited),
	}))
	if err != nil || !r.Success {
		t.Fatalf("composed write: %v %v", err, r.Error)
	}
	s := syncOf(t, r.Data)
	if !strings.Contains(fmt.Sprint(s["decomposed"]), "title.json") {
		t.Fatalf("decompose must touch title.json: %v", s)
	}
	fb, _ := os.ReadFile(filepath.Join(root, "scene.excalidraw.d", "elements", "title.json"))
	if !strings.Contains(string(fb), "Moved") {
		t.Fatalf("fragment not updated from composed: %s", fb)
	}
}

func TestSentinel_DeleteFragmentRecomposes(t *testing.T) {
	m, ctx, root := docModule(t)
	composed := seedDoc(t, root)
	r, err := m.delete(ctx, mustJSON(map[string]any{"path": "scene.excalidraw.d/elements/title.json"}))
	if err != nil || !r.Success {
		t.Fatalf("delete: %v %v", err, r.Error)
	}
	s := syncOf(t, r.Data)
	if s["composed_ok"] != true {
		t.Fatalf("compose after delete: %v", s)
	}
	b, _ := os.ReadFile(composed)
	if strings.Contains(string(b), "title") {
		t.Fatalf("deleted element still in composed: %s", b)
	}
}
