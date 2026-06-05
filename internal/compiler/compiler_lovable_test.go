package compiler_test

import (
	"os"
	"testing"
)

// TestLovable_RealAppCompiles is an exploratory test : compile the
// digitorn-lovable app from the Python daemon's builtins/ directory
// to find out exactly what the Go compiler accepts vs rejects. We
// log every diagnostic so the gaps are visible.
func TestLovable_RealAppCompiles(t *testing.T) {
	src := `C:\Users\ASUS\Documents\digitorn-bridge\packages\digitorn\builtins\digitorn-lovable`
	if _, err := os.Stat(src); err != nil {
		t.Skipf("digitorn-lovable not found at %s : %v", src, err)
	}

	c := newCompilerForFixtures(t)
	res, err := c.Compile(src)
	if err != nil {
		t.Fatalf("Compile returned error : %v", err)
	}

	errs := res.Diagnostics.Errors()
	warns := res.Diagnostics.Warnings()
	t.Logf("compile result : %d error(s), %d warning(s)", len(errs), len(warns))
	for _, d := range errs {
		t.Logf("  ERROR %s %s : %s", d.Code, d.Pos, d.Message)
	}
	for _, d := range warns {
		t.Logf("  WARN  %s %s : %s", d.Code, d.Pos, d.Message)
	}
	if !res.OK() {
		t.Fatalf("digitorn-lovable did NOT compile : %d error(s)", len(errs))
	}
	t.Log("digitorn-lovable compiles clean")
}
