package bash

import (
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/pkg/module"
)

func init() {
	module.MustRegister(func() domainmodule.Module { return New() })
}
