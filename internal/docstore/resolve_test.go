package docstore

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func layoutManifest() Manifest {
	return Manifest{
		Root: "meta.json",
		Collections: []Collection{
			{Name: "elements", Path: "/elements", ID: "id", Grain: "item", Order: "field:index"},
		},
		Layout: &Layout{
			Route: "orthogonal", Gap: 8,
			Edge:    &EdgeSpec{From: "from", To: "to", In: "elements"},
			Derived: []string{"x", "y", "width", "height", "points", "startBinding", "endBinding"},
		},
	}
}

func writeFrag(t *testing.T, dir, name, body string) {
	t.Helper()
	os.MkdirAll(filepath.Join(dir, "elements"), 0o755)
	if err := os.WriteFile(filepath.Join(dir, "elements", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func elemByID(t *testing.T, composed []byte) map[string]map[string]any {
	t.Helper()
	var d struct {
		Elements []map[string]any `json:"elements"`
	}
	if err := json.Unmarshal(composed, &d); err != nil {
		t.Fatalf("composed not JSON: %v", err)
	}
	m := map[string]map[string]any{}
	for _, e := range d.Elements {
		m[e["id"].(string)] = e
	}
	return m
}

// The core promise: the agent declares only from/to; the engine routes the
// arrow so it lands exactly on both boxes' borders — never floating, never short.
func TestResolver_ArrowLandsOnBoxEdges(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	// two stacked boxes; agent computed NOTHING for the arrow.
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":100,"y":100,"width":200,"height":80}`)
	writeFrag(t, dir, "b.json", `{"id":"b","type":"rectangle","index":"a1","x":100,"y":400,"width":200,"height":80}`)
	writeFrag(t, dir, "e.json", `{"id":"e","type":"arrow","index":"a2","from":"a","to":"b"}`)

	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v (%v)", err, diags)
	}
	els := elemByID(t, composed)
	arrow := els["e"]

	// absolute endpoints of the routed arrow
	ax, ay := arrow["x"].(float64), arrow["y"].(float64)
	pts := arrow["points"].([]any)
	last := pts[len(pts)-1].([]any)
	ex, ey := ax+last[0].(float64), ay+last[1].(float64)

	// box a bottom edge ≈ y=180, box b top edge ≈ y=400; with gap 8 the arrow
	// must start just below a (≈188) and end just above b (≈392), x centered (≈200).
	if math.Abs(ax-200) > 1 || math.Abs(ay-188) > 1 {
		t.Fatalf("start not on box a bottom-centre: (%.1f,%.1f) want ~(200,188)", ax, ay)
	}
	if math.Abs(ex-200) > 1 || math.Abs(ey-392) > 1 {
		t.Fatalf("end not on box b top-centre: (%.1f,%.1f) want ~(200,392)", ex, ey)
	}
	// bindings + back-refs auto-set
	if sb, _ := arrow["startBinding"].(map[string]any); sb["elementId"] != "a" || sb["focus"].(float64) != 0 {
		t.Fatalf("startBinding wrong: %v", arrow["startBinding"])
	}
	be, _ := els["a"]["boundElements"].([]any)
	if len(be) != 1 || be[0].(map[string]any)["id"] != "e" {
		t.Fatalf("box a missing arrow back-ref: %v", els["a"]["boundElements"])
	}
}

// Grid placement gives non-overlapping positions from logical cells.
func TestResolver_GridPlacesNoOverlap(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	m.Layout.Grid = &GridSpec{Field: "cell", CellW: 200, CellH: 80, GutterX: 100, GutterY: 100, OriginX: 0, OriginY: 0, DefaultW: 200, DefaultH: 80}
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","cell":[0,0]}`)
	writeFrag(t, dir, "b.json", `{"id":"b","type":"rectangle","index":"a1","cell":[1,0]}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	els := elemByID(t, composed)
	if els["a"]["x"].(float64) != 0 || els["b"]["x"].(float64) != 300 {
		t.Fatalf("grid x wrong: a=%v b=%v (want 0, 300)", els["a"]["x"], els["b"]["x"])
	}
	// no horizontal overlap: a ends at 200, b starts at 300
}

// Moving a box re-routes its arrows on the next compose (edge geometry is
// always recomputed from current box rects — better than a human tracking it).
func TestResolver_MovingBoxReRoutes(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":100,"y":100,"width":200,"height":80}`)
	writeFrag(t, dir, "b.json", `{"id":"b","type":"rectangle","index":"a1","x":100,"y":400,"width":200,"height":80}`)
	writeFrag(t, dir, "e.json", `{"id":"e","type":"arrow","index":"a2","from":"a","to":"b"}`)
	c1, _, _ := Compose(m, dir)
	y1 := elemByID(t, c1)["e"]["y"].(float64)

	// move box a far right — no touch to the arrow fragment at all
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":900,"y":100,"width":200,"height":80}`)
	c2, _, _ := Compose(m, dir)
	arrow := elemByID(t, c2)["e"]
	x2 := arrow["x"].(float64)
	// box a moved to x=900 (edges 900..1100); the arrow must now leave a's
	// left face (~892), not sit at its old spot (200).
	if x2 < 800 {
		t.Fatalf("arrow did not re-route to moved box (x1-start=200, x2=%.0f, y1=%.0f)", x2, y1)
	}
}

// Decompose keeps the arrow fragment declarative: the app's canvas save (full
// geometry) must not pollute the stored from/to fragment.
func TestResolver_DecomposeKeepsEdgeDeclarative(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":100,"y":100,"width":200,"height":80}`)
	writeFrag(t, dir, "b.json", `{"id":"b","type":"rectangle","index":"a1","x":100,"y":400,"width":200,"height":80}`)
	writeFrag(t, dir, "e.json", `{"id":"e","type":"arrow","index":"a2","from":"a","to":"b"}`)
	composed, _, _ := Compose(m, dir)

	j := LoadJournal(dir)
	// simulate the app saving the composed doc back (canvas write)
	if _, err := Decompose(m, composed, dir, j); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "elements", "e.json"))
	var frag map[string]any
	json.Unmarshal(b, &frag)
	if _, has := frag["points"]; has {
		t.Fatalf("edge fragment must not store computed points: %s", b)
	}
	if frag["from"] != "a" || frag["to"] != "b" {
		t.Fatalf("edge fragment lost its declarative from/to: %s", b)
	}
}

func TestComplete_FillsRenderableFields(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	m.Defaults = &Defaults{
		TypeField: "type",
		Common:    map[string]any{"isDeleted": false, "opacity": float64(100), "angle": float64(0), "groupIds": []any{}},
		ByType: map[string]map[string]any{
			"rectangle": {"strokeColor": "#1e1e1e", "backgroundColor": "transparent", "strokeWidth": float64(2), "fillStyle": "solid", "roughness": float64(1)},
		},
		Generated: map[string][]string{"hash_int": {"seed", "versionNonce"}},
	}
	writeFrag(t, dir, "box.json", `{"id":"box","type":"rectangle","index":"a0","x":10,"y":10,"width":100,"height":50}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v", err)
	}
	els := elemByID(t, composed)
	b := els["box"]
	for _, f := range []string{"isDeleted", "opacity", "angle", "groupIds", "strokeColor", "backgroundColor", "strokeWidth", "seed", "versionNonce"} {
		if _, ok := b[f]; !ok {
			t.Fatalf("completion missing field %q: %v", f, b)
		}
	}
	if b["strokeColor"] != "#1e1e1e" || b["opacity"].(float64) != 100 {
		t.Fatalf("default values wrong: %v", b)
	}
	// seed deterministic + non-zero
	if b["seed"].(float64) == 0 {
		t.Fatalf("seed not generated")
	}
	// authored fields untouched
	if b["x"].(float64) != 10 || b["width"].(float64) != 100 {
		t.Fatalf("completion clobbered authored geometry")
	}
}

func TestComplete_DeterministicSeed(t *testing.T) {
	dir := t.TempDir()
	m := layoutManifest()
	m.Defaults = &Defaults{Generated: map[string][]string{"hash_int": {"seed"}}}
	writeFrag(t, dir, "box.json", `{"id":"box","type":"rectangle","index":"a0","x":0,"y":0,"width":10,"height":10}`)
	c1, _, _ := Compose(m, dir)
	c2, _, _ := Compose(m, dir)
	s1 := elemByID(t, c1)["box"]["seed"].(float64)
	s2 := elemByID(t, c2)["box"]["seed"].(float64)
	if s1 != s2 {
		t.Fatalf("seed must be deterministic across composes: %v vs %v", s1, s2)
	}
}
