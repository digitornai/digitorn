package compiler_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/catalog"
	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

func repoManifestsDir(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	dir := wd
	for i := 0; i < 8; i++ {
		gomod := filepath.Join(dir, "go.mod")
		mani := filepath.Join(dir, "manifests")
		if _, err := os.Stat(gomod); err == nil {
			if info, err := os.Stat(mani); err == nil && info.IsDir() {
				return mani
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return ""
}

func newCompilerForFixtures(t *testing.T) *compiler.Compiler {
	t.Helper()
	c := compiler.New()
	if mani := repoManifestsDir(t); mani != "" {
		c.WithSources(catalog.DirSource{Dir: mani})
	}
	return c
}

type expectations struct {
	Description    string   `json:"description,omitempty"`
	Codes          []string `json:"codes,omitempty"`
	ForbiddenCodes []string `json:"forbidden_codes,omitempty"`
	AllowWarnings  bool     `json:"allow_warnings,omitempty"`
	Strict         bool     `json:"strict,omitempty"`
	BuildOK        bool     `json:"build_ok,omitempty"`
}

func TestFixtures(t *testing.T) {
	for _, category := range []string{"valid", "invalid"} {
		category := category
		dir := filepath.Join("testdata", category)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				t.Logf("no fixtures under %s — skipping", dir)
				continue
			}
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			name := e.Name()
			fixDir := filepath.Join(dir, name)
			t.Run(category+"/"+name, func(t *testing.T) {
				runFixture(t, category, fixDir)
			})
		}
	}
}

func runFixture(t *testing.T, category, fixDir string) {
	t.Helper()
	want := loadExpectations(t, fixDir)
	if category == "valid" && len(want.Codes) == 0 {
		want.AllowWarnings = true
	}

	c := newCompilerForFixtures(t)
	res, err := c.Compile(fixDir)
	if err != nil {
		t.Fatalf("Compile(%s): %v", fixDir, err)
	}
	if res.Diagnostics == nil {
		t.Fatalf("Compile(%s): nil diagnostics", fixDir)
	}

	got := codeSet(res.Diagnostics.All())
	gotErrors := codeSet(res.Diagnostics.Errors())
	gotWarnings := codeSet(res.Diagnostics.Warnings())

	for _, code := range want.Codes {
		if !got[code] {
			t.Errorf("%s: missing expected code %s\nobserved: %v",
				fixDir, code, sortedKeys(got))
		}
	}
	for _, code := range want.ForbiddenCodes {
		if got[code] {
			t.Errorf("%s: forbidden code %s appeared\nobserved: %v",
				fixDir, code, sortedKeys(got))
		}
	}

	switch category {
	case "valid":
		if len(gotErrors) > 0 {
			t.Errorf("%s: valid fixture produced errors: %v\nfull diags:\n%s",
				fixDir, sortedKeys(gotErrors), formatDiags(res.Diagnostics))
		}
		if !want.AllowWarnings && len(gotWarnings) > 0 {
			t.Errorf("%s: valid fixture produced warnings (AllowWarnings=false): %v",
				fixDir, sortedKeys(gotWarnings))
		}
	case "invalid":
		if len(want.Codes) == 0 {
			t.Errorf("%s: invalid fixture declared no expected codes — give it at least one in expect.json",
				fixDir)
		}
		if len(gotErrors) == 0 {
			// At least one error or warning must match the expected codes.
			haveAny := false
			for _, code := range want.Codes {
				if got[code] {
					haveAny = true
					break
				}
			}
			if !haveAny {
				t.Errorf("%s: invalid fixture produced no errors and no expected code\nobserved: %v",
					fixDir, sortedKeys(got))
			}
		}
	}

	// Strict mode : the observed set of relevant codes must equal Codes.
	if want.Strict {
		expected := setFromSlice(want.Codes)
		// Ignore warnings unless explicitly listed.
		relevant := map[string]bool{}
		for k := range gotErrors {
			relevant[k] = true
		}
		for k := range gotWarnings {
			if expected[k] {
				relevant[k] = true
			}
		}
		if !equalSets(expected, relevant) {
			t.Errorf("%s: strict mode mismatch\nexpected: %v\nobserved: %v",
				fixDir, sortedKeys(expected), sortedKeys(relevant))
		}
	}

	// Build artifact when requested.
	if want.BuildOK {
		if !res.OK() {
			t.Errorf("%s: BuildOK requested but compile has errors", fixDir)
		} else {
			art, err := c.Build(res)
			if err != nil {
				t.Errorf("%s: Build failed: %v", fixDir, err)
			} else if art == nil {
				t.Errorf("%s: Build returned nil artifact", fixDir)
			}
		}
	}
}

func loadExpectations(t *testing.T, fixDir string) expectations {
	t.Helper()
	path := filepath.Join(fixDir, "expect.json")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return expectations{}
		}
		t.Fatalf("read %s: %v", path, err)
	}
	var out expectations
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return out
}

func codeSet(diags []diagnostic.Diagnostic) map[string]bool {
	out := map[string]bool{}
	for _, d := range diags {
		out[string(d.Code)] = true
	}
	return out
}

func setFromSlice(s []string) map[string]bool {
	out := map[string]bool{}
	for _, v := range s {
		out[v] = true
	}
	return out
}

func equalSets(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func formatDiags(bag *diagnostic.Bag) string {
	var b strings.Builder
	for _, d := range bag.All() {
		b.WriteString("  ")
		b.WriteString(string(d.Code))
		b.WriteString(" [")
		b.WriteString(d.Severity.String())
		b.WriteString("] ")
		b.WriteString(d.Pos.String())
		b.WriteString(": ")
		b.WriteString(d.Message)
		b.WriteString("\n")
	}
	return b.String()
}
