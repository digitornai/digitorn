package http

import (
	"testing"

	"github.com/mbathepaul/digitorn/pkg/module"
)

// TestModuleRegistered guards against the regression where internal/modules/http
// was blank-imported by the daemon and worker but never actually registered
// (it had no init()/MustRegister), so http.* tools silently failed to load.
func TestModuleRegistered(t *testing.T) {
	if !module.Default.Has("http") {
		t.Fatal("http module is not registered in module.Default; register.go init() missing or broken")
	}

	man, ok := module.Default.Manifest("http")
	if !ok {
		t.Fatal("no manifest for http module")
	}
	if man.ID != "http" {
		t.Fatalf("manifest ID = %q, want %q", man.ID, "http")
	}

	want := map[string]bool{
		"get": false, "post": false, "put": false, "patch": false,
		"delete": false, "head": false, "request": false,
		"download": false, "upload": false,
	}
	for _, ts := range man.Tools {
		if _, expected := want[ts.Name]; expected {
			want[ts.Name] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected tool %q not present in http manifest", name)
		}
	}
}
