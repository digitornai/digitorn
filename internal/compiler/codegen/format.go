// Package codegen serializes a validated AppDefinition to .dgc — a binary
// artifact the runtime can load without re-validating the source manifest.
package codegen

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// FileMagic identifies a .dgc file. Magic + Format together gate forward
// compatibility: bumping Format breaks old readers cleanly with a clear error.
const (
	FileMagic       = "DGTC"
	FormatVersion   = uint8(2)
	CompilerVersion = "0.1.0"
)

type Header struct {
	Magic           [4]byte
	Format          uint8
	CompilerVersion string
	CompiledAt      int64
	VersionHash     string
}

type Artifact struct {
	Header     Header
	Definition *schema.AppDefinition
}
