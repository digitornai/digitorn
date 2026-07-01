package compiler_test

import (
	"path/filepath"
	"testing"
)

// TestCraft_RealAppCompiles compiles the digitorn-craft builtin app
// to verify the Go compiler accepts the full production manifest.
func TestCraft_RealAppCompiles(t *testing.T) {
	src, err := filepath.Abs("../appmgr/builtins/digitorn-craft")
	if err != nil {
		t.Fatal(err)
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
		t.Fatalf("digitorn-craft did NOT compile : %d error(s)", len(errs))
	}
	t.Log("digitorn-craft compiles clean")
}
