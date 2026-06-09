package filesystem

import "testing"

// TestEditExposesSurgicalParamsToLLM guards the contract that the LLM sees the
// FULL surgical edit interface at runtime. The tool spec the model receives is
// resolved from the live module's Manifest() (built from RegisterTool) via
// LookupToolSpec → Registry.Manifest — NOT the static manifests/filesystem.yaml.
// If a refactor narrows the registered params (or marks old_string required), the
// agent silently loses line/insert/anchor editing — this catches that.
func TestEditExposesSurgicalParamsToLLM(t *testing.T) {
	mf := New().Manifest()
	var edit *struct {
		params   map[string]bool // name -> required
		hasParam map[string]bool
	}
	for _, tl := range mf.Tools {
		if tl.Name != "edit" {
			continue
		}
		edit = &struct {
			params   map[string]bool
			hasParam map[string]bool
		}{params: map[string]bool{}, hasParam: map[string]bool{}}
		for _, p := range tl.Params {
			edit.params[p.Name] = p.Required
			edit.hasParam[p.Name] = true
		}
	}
	if edit == nil {
		t.Fatal("edit tool not registered in the live module manifest")
	}
	for _, want := range []string{
		"path", "new_string", "old_string", "replace_all", "occurrence",
		"start_line", "end_line", "insert_after", "insert_before",
		"prepend", "append", "expect", "dry_run",
	} {
		if !edit.hasParam[want] {
			t.Errorf("edit param %q is NOT exposed to the LLM at runtime", want)
		}
	}
	if edit.params["old_string"] {
		t.Errorf("old_string is REQUIRED at runtime — it must be optional (start_line / insert_* are alternatives)")
	}
	if !edit.params["path"] {
		t.Errorf("path must be required")
	}
}
