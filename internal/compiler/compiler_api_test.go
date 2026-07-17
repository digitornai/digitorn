package compiler_test

import (
	"bytes"
	"crypto/sha256"
	"path/filepath"
	"sync"
	"testing"

	"github.com/digitornai/digitorn/internal/compiler"
	"github.com/digitornai/digitorn/internal/compiler/codegen"
)

func TestCompilerAPI_BuildDeterministic(t *testing.T) {
	for _, fix := range []string{"minimal_chat", "full_featured"} {
		fix := fix
		t.Run(fix, func(t *testing.T) {
			fixPath := filepath.Join("testdata", "valid", fix)

			c1 := newCompilerForFixtures(t)
			r1, err := c1.Compile(fixPath)
			if err != nil || !r1.OK() {
				t.Fatalf("compile #1: err=%v ok=%v", err, r1 != nil && r1.OK())
			}
			a1, err := c1.Build(r1)
			if err != nil {
				t.Fatalf("build #1: %v", err)
			}

			c2 := newCompilerForFixtures(t)
			r2, err := c2.Compile(fixPath)
			if err != nil || !r2.OK() {
				t.Fatalf("compile #2: err=%v ok=%v", err, r2 != nil && r2.OK())
			}
			a2, err := c2.Build(r2)
			if err != nil {
				t.Fatalf("build #2: %v", err)
			}

			a1.Header.CompiledAt = 0
			a2.Header.CompiledAt = 0

			b1, err := codegen.EncodeBytes(a1)
			if err != nil {
				t.Fatalf("encode #1: %v", err)
			}
			b2, err := codegen.EncodeBytes(a2)
			if err != nil {
				t.Fatalf("encode #2: %v", err)
			}
			if !bytes.Equal(b1, b2) {
				h1 := sha256.Sum256(b1)
				h2 := sha256.Sum256(b2)
				t.Fatalf("artifacts differ between two compile runs of %s\nsize: %d vs %d\nsha256: %x vs %x",
					fix, len(b1), len(b2), h1, h2)
			}
		})
	}
}

func TestCompilerAPI_BuildRefusesWithErrors(t *testing.T) {
	fixPath := filepath.Join("testdata", "invalid", "duplicate_agent_id")
	c := newCompilerForFixtures(t)
	res, err := c.Compile(fixPath)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if res.OK() {
		t.Fatal("expected non-OK result for duplicate_agent_id fixture")
	}
	art, err := c.Build(res)
	if err == nil {
		t.Errorf("Build returned no error on invalid result; artifact=%v", art)
	}
	if art != nil {
		t.Errorf("Build returned non-nil artifact for invalid result")
	}
}

func TestCompilerAPI_BuildRefusesNilResult(t *testing.T) {
	c := compiler.New()
	if _, err := c.Build(nil); err == nil {
		t.Error("Build(nil) returned no error")
	}
}

func TestCompilerAPI_CompileNonexistentPath(t *testing.T) {
	c := compiler.New()
	res, err := c.Compile(filepath.Join("testdata", "this", "does", "not", "exist"))
	if err == nil && res != nil && res.OK() {
		t.Error("Compile on missing path returned OK")
	}
}

func TestCompilerAPI_ConcurrentCompile(t *testing.T) {
	c := newCompilerForFixtures(t)
	fixtures := []string{
		"testdata/valid/minimal_chat",
		"testdata/valid/full_featured",
		"testdata/invalid/duplicate_agent_id",
		"testdata/invalid/unknown_module",
		"testdata/invalid/cycle_delegate",
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, fix := range fixtures {
			wg.Add(1)
			go func(p string) {
				defer wg.Done()
				if _, err := c.Compile(p); err != nil {
					t.Errorf("compile %s: %v", p, err)
				}
			}(fix)
		}
	}
	wg.Wait()
}
