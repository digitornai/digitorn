package docstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func excalidrawManifest() Manifest {
	return Manifest{
		Match: "*.excalidraw",
		Root:  "meta.json",
		Collections: []Collection{
			{Name: "elements", Path: "/elements", ID: "id", Grain: "item", Order: "field:index"},
			{Name: "files", Path: "/files", ID: "$key", Grain: "item"},
		},
		Validate: Validate{
			UniqueID: true,
			Refs: []Ref{
				{Field: "startBinding.elementId", In: "elements"},
				{Field: "endBinding.elementId", In: "elements"},
			},
		},
	}
}

const sampleDoc = `{
  "type": "excalidraw", "version": 2,
  "appState": {"viewBackgroundColor": "#fff"},
  "elements": [
    {"id": "title", "type": "text", "index": "a0", "x": 280, "text": "Hello"},
    {"id": "center_rect", "type": "rectangle", "index": "a1", "x": 330},
    {"id": "arrow_1", "type": "arrow", "index": "a2", "startBinding": {"elementId": "center_rect"}}
  ],
  "files": {"img_9f3c": {"mimeType": "image/png", "dataURL": "data:,x"}}
}`

func mustJSONEq(t *testing.T, got, want []byte) {
	t.Helper()
	var g, w any
	if err := json.Unmarshal(got, &g); err != nil {
		t.Fatalf("got not JSON: %v", err)
	}
	if err := json.Unmarshal(want, &w); err != nil {
		t.Fatalf("want not JSON: %v", err)
	}
	if !reflect.DeepEqual(g, w) {
		t.Fatalf("documents differ\ngot:  %s\nwant: %s", got, want)
	}
}

func TestRoundTrip_ExplodeCompose(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	j, err := Explode(m, []byte(sampleDoc), dir)
	if err != nil {
		t.Fatalf("explode: %v", err)
	}
	for _, f := range []string{"meta.json", "elements/title.json", "elements/center_rect.json", "files/img_9f3c.json"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Fatalf("missing fragment %s", f)
		}
	}
	if len(j.Items) != 5 { // 3 elements + 1 file + /root
		t.Fatalf("journal items = %d, want 5 (%v)", len(j.Items), j.Items)
	}
	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v (%v)", err, diags)
	}
	mustJSONEq(t, composed, []byte(sampleDoc))
}

func TestCompose_OrdersByFractionalIndex(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	os.MkdirAll(filepath.Join(dir, "elements"), 0o755)
	write := func(name, body string) {
		os.WriteFile(filepath.Join(dir, "elements", name), []byte(body), 0o644)
	}
	write("b.json", `{"id":"b","index":"a1"}`)
	write("mid.json", `{"id":"mid","index":"a0V"}`)
	write("a.json", `{"id":"a","index":"a0"}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	var doc struct {
		Elements []struct{ ID string } `json:"elements"`
	}
	json.Unmarshal(composed, &doc)
	got := []string{doc.Elements[0].ID, doc.Elements[1].ID, doc.Elements[2].ID}
	if got[0] != "a" || got[1] != "mid" || got[2] != "b" {
		t.Fatalf("fractional order wrong: %v", got)
	}
}

func TestCompose_BrokenFragmentSkippedNotBlocking(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	os.MkdirAll(filepath.Join(dir, "elements"), 0o755)
	os.WriteFile(filepath.Join(dir, "elements", "bad.json"), []byte(`{"id":"bad", "x": }`), 0o644)
	os.WriteFile(filepath.Join(dir, "elements", "good.json"), []byte(`{"id":"good","type":"rectangle"}`), 0o644)
	composed, diags, err := Compose(m, dir)
	if err != nil || composed == nil {
		t.Fatalf("one broken fragment must NOT block compose: err=%v", err)
	}
	if _, ok := elemByID(t, composed)["good"]; !ok {
		t.Fatalf("the good fragment must still compose")
	}
	var d *Diagnostic
	for i := range diags {
		if diags[i].File == "elements/bad.json" {
			d = &diags[i]
		}
	}
	if d == nil || d.Severity != "warning" || d.Rule != "parse" || !strings.Contains(d.Message, "at byte") {
		t.Fatalf("want a parse WARNING with offset naming the file, got %+v", diags)
	}
}

func TestCompose_DanglingBindingGetsClosestHint(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	os.MkdirAll(filepath.Join(dir, "elements"), 0o755)
	os.WriteFile(filepath.Join(dir, "elements", "center_rect.json"),
		[]byte(`{"id":"center_rect","type":"rectangle"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "elements", "arrow_9.json"),
		[]byte(`{"id":"arrow_9","type":"arrow","startBinding":{"elementId":"center_rct"}}`), 0o644)
	_, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("dangling ref must warn, not block: %v", err)
	}
	found := false
	for _, d := range diags {
		if d.Rule == "refs" && d.Severity == "warning" && strings.Contains(d.Hint, "center_rect") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want refs warning with closest hint, got %+v", diags)
	}
}

func TestCompose_DanglingEdgeDroppedAndWarned(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":0,"y":0,"width":100,"height":50}`)
	writeFrag(t, dir, "e.json", `{"id":"e","type":"arrow","index":"a1","from":"a","to":"ghost"}`)
	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("dangling from/to must warn, not block: %v", err)
	}
	e := elemByID(t, composed)["e"]
	if del, _ := e["isDeleted"].(bool); !del {
		t.Fatalf("dangling edge must be marked isDeleted (no ghost stub), got %v", e["isDeleted"])
	}
	found := false
	for _, d := range diags {
		if d.Rule == "refs" && strings.Contains(d.Message, "ghost") {
			found = true
		}
	}
	if !found {
		t.Fatalf("want a refs warning naming the dangling target, got %+v", diags)
	}
}

func TestCompose_DuplicateIDSkippedNotBlocking(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	os.MkdirAll(filepath.Join(dir, "elements"), 0o755)
	os.WriteFile(filepath.Join(dir, "elements", "one.json"), []byte(`{"id":"dup","type":"rectangle"}`), 0o644)
	os.WriteFile(filepath.Join(dir, "elements", "two.json"), []byte(`{"id":"dup","type":"ellipse"}`), 0o644)
	composed, diags, err := Compose(m, dir)
	if err != nil || composed == nil {
		t.Fatalf("duplicate id must warn, not block: %v", err)
	}
	if n := len(elemByID(t, composed)); n != 1 {
		t.Fatalf("duplicate must be dropped, want 1 element, got %d", n)
	}
	found := false
	for _, d := range diags {
		if d.Rule == "unique_id" && d.Severity == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want unique_id warning, got %+v", diags)
	}
}

func TestDecompose_OnlyChangedFragmentTouched(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	j, err := Explode(m, []byte(sampleDoc), dir)
	if err != nil {
		t.Fatalf("explode: %v", err)
	}
	titleBefore, _ := os.ReadFile(filepath.Join(dir, "elements", "title.json"))

	var doc map[string]any
	json.Unmarshal([]byte(sampleDoc), &doc)
	els := doc["elements"].([]any)
	els[1].(map[string]any)["x"] = float64(999) // move center_rect
	edited, _ := json.Marshal(doc)

	changed, err := Decompose(m, edited, dir, j)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if len(changed) != 1 || changed[0] != "elements/center_rect.json" {
		t.Fatalf("want only center_rect changed, got %v", changed)
	}
	titleAfter, _ := os.ReadFile(filepath.Join(dir, "elements", "title.json"))
	if string(titleBefore) != string(titleAfter) {
		t.Fatalf("untouched fragment was rewritten")
	}
	b, _ := os.ReadFile(filepath.Join(dir, "elements", "center_rect.json"))
	if !strings.Contains(string(b), "999") {
		t.Fatalf("edit not applied to fragment: %s", b)
	}
}

func TestDecompose_EchoIsNoop(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	j, _ := Explode(m, []byte(sampleDoc), dir)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	RecordComposed(j, composed, m, dir)
	changed, err := Decompose(m, composed, dir, j)
	if err != nil || len(changed) != 0 {
		t.Fatalf("echo must be a no-op, got changed=%v err=%v", changed, err)
	}
}

func TestDecompose_RemovalDeletesFragment(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	j, _ := Explode(m, []byte(sampleDoc), dir)

	var doc map[string]any
	json.Unmarshal([]byte(sampleDoc), &doc)
	doc["elements"] = doc["elements"].([]any)[:2] // drop arrow_1
	edited, _ := json.Marshal(doc)

	changed, err := Decompose(m, edited, dir, j)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "elements", "arrow_1.json")); !os.IsNotExist(statErr) {
		t.Fatalf("removed item's fragment must be deleted (changed=%v)", changed)
	}
}

func TestDecompose_NewItemWithoutIDGetsGenerated(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	j, _ := Explode(m, []byte(sampleDoc), dir)

	var doc map[string]any
	json.Unmarshal([]byte(sampleDoc), &doc)
	doc["elements"] = append(doc["elements"].([]any), map[string]any{"type": "ellipse", "x": 5})
	edited, _ := json.Marshal(doc)

	changed, err := Decompose(m, edited, dir, j)
	if err != nil {
		t.Fatalf("decompose: %v", err)
	}
	genFound := false
	for _, c := range changed {
		if strings.HasPrefix(c, "elements/gen_") {
			genFound = true
		}
	}
	if !genFound {
		t.Fatalf("new id-less item must get a generated-id fragment, changed=%v", changed)
	}
}

func TestOverview_CanvasProfile(t *testing.T) {
	dir := t.TempDir()
	m := excalidrawManifest()
	m.Overview = "canvas"
	if _, err := Explode(m, []byte(sampleDoc), dir); err != nil {
		t.Fatalf("explode: %v", err)
	}
	if err := GenerateOverview(m, dir); err != nil {
		t.Fatalf("overview: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "_index", "overview.md"))
	if err != nil {
		t.Fatalf("overview.md missing: %v", err)
	}
	s := string(b)
	for _, want := range []string{"elements — 3 item(s)", "Spatial grid", "arrow_1: center_rect", "Z-order", "title(a0)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("overview missing %q:\n%s", want, s)
		}
	}
}
