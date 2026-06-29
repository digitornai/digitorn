package runtime_test

import (
	"testing"

	"github.com/mbathepaul/digitorn/internal/compiler"
	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
	_ "github.com/mbathepaul/digitorn/internal/modules/http"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// TestGLPISupportCompiles is a non-live guard: the flagship examples/glpi-support
// app must compile clean against the real module catalog. It exercises the http
// module registration (flow tool nodes http.post/http.put), the channels
// background module, the flow schema, and the {{include:kb/*.md}} context.
func TestGLPISupportCompiles(t *testing.T) {
	c := compiler.New().WithSources(catalog.RegistrySource{Registry: module.Default})
	res, err := c.Compile("../../examples/glpi-support")
	if err != nil {
		t.Fatalf("compile glpi-support: %v", err)
	}
	if !res.OK() {
		t.Fatalf("glpi-support must compile clean:\n%v", res.Diagnostics)
	}
	if res.Definition.Flow == nil {
		t.Fatal("glpi-support must define a flow")
	}
	if got := len(res.Definition.Agents); got != 4 {
		t.Errorf("expected 4 agents (triage + 3 specialists), got %d", got)
	}
}
