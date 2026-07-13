package docstore

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func painterManifest() Manifest {
	m := layoutManifest()
	m.Layout.Grid = &GridSpec{Field: "cell", CellW: 200, CellH: 80, GutterX: 100, GutterY: 100, DefaultW: 200, DefaultH: 80}
	m.Layout.Path = &PathSpec{Field: "path", Box: "box", Type: "freedraw", Step: 6}
	m.Layout.Frame = &FrameSpec{Contains: "contains", Pad: 20}
	m.Layout.GroupField = "group"
	return m
}

// The painter promise: SVG path in, perfectly fitted point strokes out.
func TestPaths_CurveSampledAndFittedToBox(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	// a hill: quadratic curve authored in a 100×80 space, target box 400×200 at (50,60)
	writeFrag(t, dir, "hill.json", `{"id":"hill","index":"a0","path":"M0,80 Q50,0 100,80","box":[50,60,400,200],"strokeColor":"#2f9e44"}`)

	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v (%v)", err, diags)
	}
	els := elemByID(t, composed)
	h := els["hill"]
	if h["type"] != "freedraw" {
		t.Fatalf("stroke type: %v", h["type"])
	}
	pts := h["points"].([]any)
	if len(pts) < 9 {
		t.Fatalf("curve under-sampled: %d points", len(pts))
	}
	// curve bbox is 100×40 (apex y=40, control point not reached) → uniform
	// scale ×4 into 400×200 → 400×160, centred vertically at y=80
	w, hh := h["width"].(float64), h["height"].(float64)
	if math.Abs(w-400) > 1 || math.Abs(hh-160) > 1 {
		t.Fatalf("bad fit: w=%.1f h=%.1f want 400×160", w, hh)
	}
	x, y := h["x"].(float64), h["y"].(float64)
	if math.Abs(x-50) > 1 || math.Abs(y-80) > 1 {
		t.Fatalf("not centred in box: x=%.1f y=%.1f want (50,80)", x, y)
	}
}

// Multi-subpath figure: extra subpaths become generated sibling strokes that
// inherit style, and are never persisted as fragments on decompose.
func TestPaths_MultiStrokeFigure(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	writeFrag(t, dir, "cat.json", `{"id":"cat","index":"a0","path":"M0,50 L100,50 M20,0 L30,20 M80,0 L70,20","box":[0,0,300,150],"strokeColor":"#e03131"}`)

	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	els := elemByID(t, composed)
	if len(els) != 3 {
		t.Fatalf("want 3 strokes (1 authored + 2 generated), got %d", len(els))
	}
	s1 := els["cat__s1"]
	if s1 == nil || s1["strokeColor"] != "#e03131" {
		t.Fatalf("generated stroke missing or lost style: %v", s1)
	}
	// z-order: siblings sort right after the parent
	if s1["index"] != "a0V1" {
		t.Fatalf("sibling index: %v", s1["index"])
	}

	j := LoadJournal(dir)
	if _, err := Decompose(m, composed, dir, j); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "elements", "cat__s1.json")); !os.IsNotExist(err) {
		t.Fatalf("generated stroke leaked to a fragment file")
	}
}

// A path fragment placed on the grid draws inside its cell — no box needed.
func TestPaths_CellIsTheBox(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	writeFrag(t, dir, "fig.json", `{"id":"fig","index":"a0","cell":[1,0],"path":"M0,0 L100,100"}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	f := elemByID(t, composed)["fig"]
	// cell [1,0] → x=300 (200+100 gutter), w=200 h=80 → uniform scale 0.8 → 80×80 centred
	x := f["x"].(float64)
	if x < 300 || x > 500 {
		t.Fatalf("stroke not inside its cell: x=%.1f", x)
	}
}

// Arcs and closes parse; garbage refuses composition with a helpful diagnostic.
func TestPaths_ArcAndInvalid(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	writeFrag(t, dir, "circle.json", `{"id":"circle","index":"a0","path":"M0,50 A50,50 0 1 1 0,49.9 Z","box":[0,0,100,100]}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	c := elemByID(t, composed)["circle"]
	if len(c["points"].([]any)) < 10 {
		t.Fatalf("arc under-sampled")
	}

	writeFrag(t, dir, "bad.json", `{"id":"bad","index":"a1","path":"M0,0 X99"}`)
	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("invalid path must warn, not block: %v", err)
	}
	bad := elemByID(t, composed)["bad"]
	if del, _ := bad["isDeleted"].(bool); !del {
		t.Fatalf("bad-path stroke must be marked isDeleted, got %v", bad["isDeleted"])
	}
	found := false
	for _, d := range diags {
		if d.Rule == "path" && d.Severity == "warning" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing path warning: %v", diags)
	}
}

// Frames auto-size around members (+pad) and stamp frameId; groups expand.
func TestFrames_AutoSizeAndGroups(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	writeFrag(t, dir, "a.json", `{"id":"a","type":"rectangle","index":"a0","x":100,"y":100,"width":200,"height":80,"group":"g1"}`)
	writeFrag(t, dir, "b.json", `{"id":"b","type":"rectangle","index":"a1","x":400,"y":300,"width":200,"height":80,"group":"g1"}`)
	writeFrag(t, dir, "fr.json", `{"id":"fr","type":"frame","index":"a2","contains":["a","b"]}`)

	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	els := elemByID(t, composed)
	fr := els["fr"]
	// members span (100,100)-(600,380); +20 pad → (80,80) 540×320
	if fr["x"].(float64) != 80 || fr["y"].(float64) != 80 ||
		fr["width"].(float64) != 540 || fr["height"].(float64) != 320 {
		t.Fatalf("frame not sized to members+pad: x=%v y=%v w=%v h=%v",
			fr["x"], fr["y"], fr["width"], fr["height"])
	}
	if els["a"]["frameId"] != "fr" || els["b"]["frameId"] != "fr" {
		t.Fatalf("members not stamped with frameId")
	}
	ga := els["a"]["groupIds"].([]any)
	if len(ga) != 1 || ga[0] != "g1" {
		t.Fatalf("group not expanded: %v", ga)
	}
}

// Shared view: separate fragments keep their true relative positions and
// proportions — the core unlock for multi-stroke figurative art.
func TestPaths_SharedViewAligns(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	m.Layout.Path.View = "view"
	m.Layout.Path.Canvas = [4]float64{0, 0, 400, 400}
	// two features in the SAME view; a big head and a tiny eye at (120,180)
	writeFrag(t, dir, "head.json", `{"id":"head","index":"a0","view":[0,0,400,400],"path":"M20,200 L380,200"}`)
	writeFrag(t, dir, "eye.json", `{"id":"eye","index":"a1","view":[0,0,400,400],"path":"M120,180 L140,180"}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	els := elemByID(t, composed)
	// view == canvas (identity): head spans x 20..380, eye spans 120..140.
	// The eye must stay TINY (width ~20) and at its place — NOT rescaled to fill.
	ew := els["eye"]["width"].(float64)
	if ew < 15 || ew > 25 {
		t.Fatalf("shared view broke: eye width=%.1f, want ~20 (rescaled to its own box?)", ew)
	}
	hw := els["head"]["width"].(float64)
	if hw < 350 {
		t.Fatalf("head width=%.1f, want ~360", hw)
	}
	// eye sits to the left of head centre (200), as authored
	if els["eye"]["x"].(float64) > 150 {
		t.Fatalf("eye misplaced: x=%.1f", els["eye"]["x"])
	}
}

// A degenerate lone move-to is a WARNING (stroke skipped), not a compose-killer —
// one stray dot in a 40-stroke portrait must not blank the canvas.
func TestPaths_EmptyPathIsWarningNotError(t *testing.T) {
	dir := t.TempDir()
	m := painterManifest()
	writeFrag(t, dir, "good.json", `{"id":"good","index":"a0","view":[0,0,100,100],"path":"M0,0 L100,100"}`)
	writeFrag(t, dir, "dot.json", `{"id":"dot","index":"a1","view":[0,0,100,100],"path":"M50,50"}`)
	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("a lone move-to must NOT refuse composition: %v", err)
	}
	if _, ok := elemByID(t, composed)["good"]; !ok {
		t.Fatal("the good stroke should still compose")
	}
	warned := false
	for _, d := range diags {
		if d.Rule == "path" && d.Severity == "warning" {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("expected a path warning for the empty stroke, got %v", diags)
	}
}
