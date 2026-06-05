package codegen

import (
	"bytes"
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// TestEncode_Deterministic_MinimalDef encodes the same minimal in-memory
// AppDefinition 10 times and asserts every encoding is byte-identical.
// This isolates the codec from the rest of the compiler.
func TestEncode_Deterministic_MinimalDef(t *testing.T) {
	def := &schema.AppDefinition{
		SchemaVersion: 2,
		App: schema.AppMeta{
			AppID:   "test",
			Name:    "T",
			Version: "0.0.1",
		},
	}
	a := &Artifact{
		Header: Header{
			Magic:           [4]byte{FileMagic[0], FileMagic[1], FileMagic[2], FileMagic[3]},
			Format:          FormatVersion,
			CompilerVersion: CompilerVersion,
			CompiledAt:      1700000000,
			VersionHash:     "deadbeef",
		},
		Definition: def,
	}

	first, err := EncodeBytes(a)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 1; i < 10; i++ {
		next, err := EncodeBytes(a)
		if err != nil {
			t.Fatalf("encode #%d: %v", i, err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("encoding #%d differs from #0\nfirst: %x\nnext:  %x", i, first, next)
		}
	}
}

// TestEncode_Deterministic_WithMaps stresses the map-sorting code path.
func TestEncode_Deterministic_WithMaps(t *testing.T) {
	mods := map[string]schema.ModuleBlock{
		"zzz_module": {Config: map[string]any{"k": "v"}},
		"aaa_module": {Config: map[string]any{"x": 1, "y": 2}},
		"mmm_module": {},
	}
	def := &schema.AppDefinition{
		SchemaVersion: 2,
		App:           schema.AppMeta{AppID: "t", Name: "T", Version: "0.0.1"},
		Tools:         &schema.ToolsBlock{Modules: mods},
	}
	a := &Artifact{
		Header: Header{
			Magic:           [4]byte{FileMagic[0], FileMagic[1], FileMagic[2], FileMagic[3]},
			Format:          FormatVersion,
			CompilerVersion: CompilerVersion,
			CompiledAt:      1700000000,
			VersionHash:     "deadbeef",
		},
		Definition: def,
	}

	first, err := EncodeBytes(a)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	for i := 1; i < 20; i++ {
		next, err := EncodeBytes(a)
		if err != nil {
			t.Fatalf("encode #%d: %v", i, err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("encoding #%d differs from #0", i)
		}
	}
}
