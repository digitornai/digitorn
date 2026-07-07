
package codegen

import (
	"github.com/digitornai/digitorn/internal/compiler/schema"
)


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
