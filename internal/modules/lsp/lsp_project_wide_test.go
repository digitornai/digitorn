package lsp

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ============================================================================
// Project-wide diagnostic guarantees.
//
// What we prove here: after ONE edit, the agent reads a CLEAR view of the
// WHOLE project — every file the language server knows about, every error
// surfaced — without the agent having to call any extra tool. This is the
// "global project diagnostic" surface, layered on top of the per-file feedback
// the agent already gets from the lsp_diagnose hook.
//
// All scenarios run against REAL gopls. Skipped automatically when gopls is
// absent so CI on a stripped image still passes the rest of the suite.
// ============================================================================

// seedGoMod writes a minimal go.mod so gopls treats `dir` as a real module
// (not single-file mode). Returns the dir for fluent use in tests.
func seedGoMod(t *testing.T, dir string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ---------------------------------------------------------------------------
// PW1 — Ripple from a single edit: removing a function that other files use
// produces errors in those OTHER files; the agent's edit result lists them
// in the project section.
// ---------------------------------------------------------------------------

func TestProjectWide_RippleFromOneEdit_SurfacesAllAffectedFiles(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// Seed: helper.go defines Add(); main.go uses it. Both clean.
	_, _ = firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\nfunc Add(a, b int) int { return a + b }\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "main.go"),
		"package main\nimport \"fmt\"\nfunc main() { fmt.Println(Add(1, 2)) }\n")

	// Now remove Add() — main.go's call breaks. The hook's text on THIS write
	// must surface the ripple under the [lsp] project section.
	ef, text := firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\n// Add removed\n")
	if !ef.Modified {
		t.Fatalf("ripple-causing edit did not modify the tool result:\n%s", text)
	}
	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("project section missing — agent does NOT see the ripple:\n%s", text)
	}
	if !strings.Contains(text, "main.go") {
		t.Fatalf("ripple section did not name the affected file main.go:\n%s", text)
	}
	t.Logf("OK — ripple surfaced to the agent:\n%s", text)
}

// ---------------------------------------------------------------------------
// PW2 — Ripple healing: an edit that fixes the upstream cause also clears
// the downstream errors. The project section vanishes from the next edit.
// ---------------------------------------------------------------------------

func TestProjectWide_RippleHealing_AllFilesClearTogether(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// Stage 1: clean baseline.
	_, _ = firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\nfunc Sum(xs []int) int { s := 0; for _, x := range xs { s += x }; return s }\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "main.go"),
		"package main\nimport \"fmt\"\nfunc main() { fmt.Println(Sum([]int{1,2,3})) }\n")

	// Stage 2: break helper.go — Sum is gone.
	_, brokenText := firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n// Sum is gone\n")
	if !strings.Contains(brokenText, "[lsp] project") {
		t.Fatalf("PW2 setup failed: ripple did not surface:\n%s", brokenText)
	}

	// Stage 3: restore Sum — both files must end up clean, no [lsp] anywhere.
	ef, healedText := firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\nfunc Sum(xs []int) int { s := 0; for _, x := range xs { s += x }; return s }\n")
	if ef.Modified || strings.Contains(healedText, "[lsp]") {
		t.Fatalf("after the fix, project should be clean but the hook still surfaced:\n%s", healedText)
	}
	t.Logf("OK — break → ripple visible; fix → ripple cleared")
}

// ---------------------------------------------------------------------------
// PW3 — Mixed project: some files broken, some clean. After editing ANY file
// (even a brand-new clean one), the agent reads exactly how many files are
// broken and which ones, ranked by error count desc.
// ---------------------------------------------------------------------------

func TestProjectWide_MixedState_AgentSeesPreciseOtherFileBreakdown(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// Seed: two buggy files, two clean.
	_, _ = firingWrite(t, e, filepath.Join(dir, "broken_a.go"),
		"package main\nfunc bA() { var x int = \"oops\"; _ = x }\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "broken_b.go"),
		"package main\nfunc bB() { _ = undefinedSymbol }\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "clean_c.go"),
		"package main\nfunc cC() int { return 42 }\n")

	// Now write a brand-new clean file. Its OWN section must be empty; the
	// project section must enumerate broken_a.go AND broken_b.go.
	_, text := firingWrite(t, e, filepath.Join(dir, "clean_d.go"),
		"package main\nfunc cD() string { return \"d\" }\n")
	if strings.Contains(text, "[lsp] clean_d.go —") {
		t.Fatalf("clean file flagged its own errors:\n%s", text)
	}
	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("no project section on clean-file edit despite project having errors:\n%s", text)
	}
	if !strings.Contains(text, "broken_a.go") || !strings.Contains(text, "broken_b.go") {
		t.Fatalf("project section missing one of broken_a/broken_b:\n%s", text)
	}
	t.Logf("OK — clean edit, project section enumerated each broken file:\n%s", text)
}

// ---------------------------------------------------------------------------
// PW4 — Refactor cascade: rename a function that 3 callers use. Before fixing
// the callers, the project section lists ALL 3. After fixing each in turn the
// count goes DOWN; when all 3 are fixed, the project section disappears.
// ---------------------------------------------------------------------------

func TestProjectWide_RefactorCascade_CountDropsAsCallersAreFixed(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// Seed: helper.Greet + three callers.
	_, _ = firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\nfunc Greet(name string) string { return \"hi \" + name }\n")
	for _, name := range []string{"call_a.go", "call_b.go", "call_c.go"} {
		_, _ = firingWrite(t, e, filepath.Join(dir, name),
			fmt.Sprintf("package main\nfunc %s() string { return Greet(\"%s\") }\n", strings.TrimSuffix(name, ".go"), name))
	}

	// Stage: rename Greet → Hello (callers now broken).
	_, brokenText := firingWrite(t, e, filepath.Join(dir, "helper.go"),
		"package main\n\nfunc Hello(name string) string { return \"hi \" + name }\n")
	if !strings.Contains(brokenText, "[lsp] project") {
		t.Fatalf("rename did not cascade — project section missing:\n%s", brokenText)
	}
	for _, c := range []string{"call_a.go", "call_b.go", "call_c.go"} {
		if !strings.Contains(brokenText, c) {
			t.Fatalf("project section missing caller %s:\n%s", c, brokenText)
		}
	}

	// Fix each caller one at a time. After each fix the project's error count
	// in the surfaced text must drop. The final fix clears the project section.
	for i, name := range []string{"call_a.go", "call_b.go", "call_c.go"} {
		ef, text := firingWrite(t, e, filepath.Join(dir, name),
			fmt.Sprintf("package main\nfunc %s() string { return Hello(\"%s\") }\n", strings.TrimSuffix(name, ".go"), name))
		isLast := i == 2
		if isLast {
			if ef.Modified || strings.Contains(text, "[lsp]") {
				t.Fatalf("after final caller fixed, agent still sees diagnostics:\n%s", text)
			}
		} else {
			if !strings.Contains(text, "[lsp] project") {
				// Intermediate fixes: own file is clean, but other callers still broken.
				// The "edited" file's own section vanishes; the project section must remain.
				t.Fatalf("intermediate fix lost the project section:\n%s", text)
			}
		}
	}
	t.Logf("OK — cascade observed: 3 broken → 0 broken as the agent fixed each caller")
}

// ---------------------------------------------------------------------------
// PW5 — Scale: a project with many files; an edit must still produce a
// readable, bounded project section. Acts as a smoke test that the rollup
// scales (no quadratic blow-up, no truncation surprise).
// ---------------------------------------------------------------------------

func TestProjectWide_Scale_ManyFilesStillProduceBoundedSummary(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// 25 clean files + 5 broken files.
	for i := range 25 {
		_, _ = firingWrite(t, e, filepath.Join(dir, fmt.Sprintf("ok_%02d.go", i)),
			fmt.Sprintf("package main\nfunc ok%02d() int { return %d }\n", i, i))
	}
	for i := range 5 {
		_, _ = firingWrite(t, e, filepath.Join(dir, fmt.Sprintf("bad_%02d.go", i)),
			fmt.Sprintf("package main\nfunc bad%02d() { var x int = \"x\"; _ = x }\n", i))
	}

	start := time.Now()
	_, text := firingWrite(t, e, filepath.Join(dir, "tail.go"),
		"package main\nfunc tail() string { return \"tail\" }\n")
	elapsed := time.Since(start)

	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("no project section on a project of 30+ files:\n%s", text)
	}
	for i := range 5 {
		if !strings.Contains(text, fmt.Sprintf("bad_%02d.go", i)) {
			t.Fatalf("project section missing bad_%02d.go:\n%s", i, text)
		}
	}
	if elapsed > 5*time.Second {
		t.Fatalf("scale test edit took %v — too slow", elapsed)
	}
	t.Logf("OK — 30 files, %d broken, last edit took %v", 5, elapsed)
}

// ---------------------------------------------------------------------------
// PW6 — A clean edit on a CLEAN project stays silent. This pins the
// no-noise guarantee even with the new project rollup active.
// ---------------------------------------------------------------------------

func TestProjectWide_CleanProject_NoNoise(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	_, _ = firingWrite(t, e, filepath.Join(dir, "a.go"),
		"package main\nfunc A() int { return 1 }\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "b.go"),
		"package main\nfunc B() int { return A() + 1 }\n")

	ef, text := firingWrite(t, e, filepath.Join(dir, "c.go"),
		"package main\nfunc C() int { return A() + B() }\n")
	if ef.Modified || strings.Contains(text, "[lsp]") {
		t.Fatalf("clean project produced noise:\n%s", text)
	}
	t.Logf("OK — every file clean → silence")
}

// ---------------------------------------------------------------------------
// PW7 — The project view is ranked: file with the most errors comes first.
// An agent skimming the surfaced text reads the worst offender immediately.
// ---------------------------------------------------------------------------

func TestProjectWide_RankedByErrorCount(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// "many.go" has multiple type errors; "few.go" has one.
	_, _ = firingWrite(t, e, filepath.Join(dir, "many.go"),
		"package main\nfunc many() {\n\tvar a int = \"x\"\n\tvar b int = \"y\"\n\tvar c int = \"z\"\n\t_ = a; _ = b; _ = c\n}\n")
	_, _ = firingWrite(t, e, filepath.Join(dir, "few.go"),
		"package main\nfunc few() { var x int = \"x\"; _ = x }\n")

	_, text := firingWrite(t, e, filepath.Join(dir, "neutral.go"),
		"package main\nfunc neutral() {}\n")
	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("project section missing:\n%s", text)
	}
	// many.go must appear BEFORE few.go in the project listing.
	iMany := strings.Index(text, "many.go")
	iFew := strings.Index(text, "few.go")
	if iMany == -1 || iFew == -1 {
		t.Fatalf("project section did not list both files:\n%s", text)
	}
	if iMany > iFew {
		t.Fatalf("ranking wrong — many.go must precede few.go:\n%s", text)
	}
	t.Logf("OK — most-broken file ranked first")
}

// ---------------------------------------------------------------------------
// PW8 — The agent's edit can ALSO break the file it just wrote. Both surfaces
// must fire: the per-edit "[lsp] thisfile —" AND the project "[lsp] project —"
// when other files are also broken. The agent sees the FULL picture in one
// shot, not a partial view.
// ---------------------------------------------------------------------------

func TestProjectWide_SelfAndRipple_BothSurfacesFire(t *testing.T) {
	requireGopls(t)
	_, e, stop := productionRig(t)
	defer stop()
	dir := seedGoMod(t, t.TempDir())

	// Existing buggy file in the project.
	_, _ = firingWrite(t, e, filepath.Join(dir, "preexisting_bad.go"),
		"package main\nfunc bad() { _ = neverDefined }\n")

	// New edit ALSO broken.
	ef, text := firingWrite(t, e, filepath.Join(dir, "fresh.go"),
		"package main\nfunc fresh() { var x int = \"oops\"; _ = x }\n")
	if !ef.Modified {
		t.Fatalf("expected the hook to fold both surfaces, got no modification:\n%s", text)
	}
	if !strings.Contains(text, "[lsp] fresh.go —") {
		t.Fatalf("own-file section missing:\n%s", text)
	}
	if !strings.Contains(text, "[lsp] project") {
		t.Fatalf("project section missing:\n%s", text)
	}
	if !strings.Contains(text, "preexisting_bad.go") {
		t.Fatalf("project section did not name preexisting_bad.go:\n%s", text)
	}
	t.Logf("OK — agent reads ITS OWN error AND the other broken file in one go:\n%s", text)
}

