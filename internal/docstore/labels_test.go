package docstore

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func labelManifest() Manifest {
	m := layoutManifest()
	m.Layout.Grid = &GridSpec{Field: "cell", CellW: 200, CellH: 80, GutterX: 100, GutterY: 100, DefaultW: 200, DefaultH: 80}
	m.Layout.Label = &LabelSpec{Field: "label", TextKey: "text", In: "elements", Type: "text", Align: "center", VAlign: "middle", FontSize: 20}
	m.RootDefaults = map[string]any{"type": "excalidraw", "version": float64(2), "source": "digitorn"}
	m.Defaults = &Defaults{
		TypeField: "type",
		ByType:    map[string]map[string]any{"text": {"fontFamily": float64(1), "lineHeight": 1.25}},
		Generated: map[string][]string{"hash_int": {"seed"}},
	}
	return m
}

// The agent declares only a label; the engine materialises a bound, centred
// text element wired both ways and z-ordered above its node.
func TestLabels_MaterialisedBoundAndOnTop(t *testing.T) {
	dir := t.TempDir()
	m := labelManifest()
	writeFrag(t, dir, "box.json", `{"id":"box","type":"rectangle","index":"a0","cell":[0,0],"label":{"text":"Auth Service"}}`)

	composed, diags, err := Compose(m, dir)
	if err != nil {
		t.Fatalf("compose: %v (%v)", err, diags)
	}
	var doc struct {
		Type     string           `json:"type"`
		Version  float64          `json:"version"`
		Elements []map[string]any `json:"elements"`
	}
	if err := json.Unmarshal(composed, &doc); err != nil {
		t.Fatal(err)
	}
	// root header seeded
	if doc.Type != "excalidraw" || doc.Version != 2 {
		t.Fatalf("root header not seeded: type=%q version=%v", doc.Type, doc.Version)
	}
	// exactly one box + one generated label
	if len(doc.Elements) != 2 {
		t.Fatalf("want box+label = 2 elements, got %d", len(doc.Elements))
	}
	var box, label map[string]any
	for _, e := range doc.Elements {
		if e["type"] == "text" {
			label = e
		} else {
			box = e
		}
	}
	if label == nil {
		t.Fatal("no text element generated for the label")
	}
	if label["text"] != "Auth Service" || label["containerId"] != "box" {
		t.Fatalf("label text/containerId wrong: %v", label)
	}
	if label["textAlign"] != "center" || label["verticalAlign"] != "middle" {
		t.Fatalf("label not centred: %v", label)
	}
	// completion reached the generated element
	if label["fontFamily"] == nil || label["seed"] == nil {
		t.Fatalf("generated label not completed by defaults: %v", label)
	}
	// z-order: label drawn AFTER (above) its box, and box lists it in boundElements
	if doc.Elements[0]["type"] == "text" {
		t.Fatalf("label must sort after its box, not before")
	}
	be, _ := box["boundElements"].([]any)
	found := false
	for _, e := range be {
		if mm, ok := e.(map[string]any); ok && mm["id"] == "box__label" && mm["type"] == "text" {
			found = true
		}
	}
	if !found {
		t.Fatalf("box missing text back-ref in boundElements: %v", box["boundElements"])
	}
}

// A generated label is never persisted as a hand-authored fragment when the app
// saves the composed doc back.
func TestLabels_NotDecomposedToFragment(t *testing.T) {
	dir := t.TempDir()
	m := labelManifest()
	writeFrag(t, dir, "box.json", `{"id":"box","type":"rectangle","index":"a0","cell":[0,0],"label":{"text":"Hi"}}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	j := LoadJournal(dir)
	if _, err := Decompose(m, composed, dir, j); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "elements", "box__label.json")); !os.IsNotExist(err) {
		t.Fatalf("generated label leaked to a fragment file")
	}
}

// A lossy canvas round-trip (app drops unknown fields cell/label) must not erase
// the agent's declarative intent — preserveAuthored restores it on decompose.
func TestLabels_AuthoredFieldsSurviveLossyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	m := labelManifest()
	writeFrag(t, dir, "box.json", `{"id":"box","type":"rectangle","index":"a0","cell":[0,0],"label":{"text":"Keep me"}}`)
	composed, _, err := Compose(m, dir)
	if err != nil {
		t.Fatal(err)
	}
	j := LoadJournal(dir)
	RecordComposed(j, composed, m, dir)

	// simulate the app re-saving the box WITHOUT the app-unknown cell/label fields
	lossy := []byte(`{"elements":[{"id":"box","type":"rectangle","index":"a0","x":0,"y":0,"width":200,"height":80}],"files":{}}`)
	if _, err := Decompose(m, lossy, dir, j); err != nil {
		t.Fatalf("decompose: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "elements", "box.json"))
	var frag map[string]any
	json.Unmarshal(b, &frag)
	if frag["cell"] == nil {
		t.Fatalf("cell erased by lossy round-trip: %s", b)
	}
	if frag["label"] == nil {
		t.Fatalf("label erased by lossy round-trip: %s", b)
	}
}
