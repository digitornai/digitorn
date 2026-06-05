package compiler_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
)

// TestStrictMode_CatchesMissingEnvVar proves that Compiler.Strict=true
// rejects a manifest that references an env var not present in the
// process environment, while the default lenient mode silently lets
// it pass (matches Python compiler).
func TestStrictMode_CatchesMissingEnvVar(t *testing.T) {
	fixPath := filepath.Join("testdata", "invalid_strict", "missing_env_var")
	envName := "THIS_ENV_VAR_DOES_NOT_EXIST_DIGITORN_STRICT_TEST"

	// Sanity : the env var must NOT exist for this test to be valid.
	if _, ok := os.LookupEnv(envName); ok {
		t.Skipf("test prerequisite : %s must be unset", envName)
	}

	t.Run("lenient_compiles_clean", func(t *testing.T) {
		c := newCompilerForFixtures(t) // Strict default = false
		res, err := c.Compile(fixPath)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		// Lenient mode : passthrough, no error. The {{env.X}} sticks
		// around verbatim for the runtime to resolve.
		if !res.OK() {
			t.Errorf("lenient compile produced errors: %s",
				formatDiags(res.Diagnostics))
		}
	})

	t.Run("strict_emits_DGT_E0201", func(t *testing.T) {
		c := newCompilerForFixtures(t)
		c.Strict = true
		res, err := c.Compile(fixPath)
		if err != nil {
			t.Fatalf("strict compile: %v", err)
		}
		if res.OK() {
			t.Fatal("strict mode must reject manifest with missing env var")
		}
		found := false
		for _, d := range res.Diagnostics.Errors() {
			if d.Code == diagnostic.CodeMissingEnvVar {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected DGT-E0201 (CodeMissingEnvVar) in strict mode, got:\n%s",
				formatDiags(res.Diagnostics))
		}
	})

	t.Run("strict_passes_when_env_set", func(t *testing.T) {
		t.Setenv(envName, "fake-value")
		c := newCompilerForFixtures(t)
		c.Strict = true
		res, err := c.Compile(fixPath)
		if err != nil {
			t.Fatalf("strict compile: %v", err)
		}
		if !res.OK() {
			t.Errorf("strict mode with env set should compile clean:\n%s",
				formatDiags(res.Diagnostics))
		}
	})

	// Reference the catalog import so the file compiles even if other
	// helpers don't reference it.
	_ = catalog.DirSource{}
}
