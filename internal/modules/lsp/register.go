package lsp

import (
	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/pkg/module"
)

func init() {
	module.MustRegister(func() domainmodule.Module { return New() })
}
