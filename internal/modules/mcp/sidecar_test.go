package mcp

import "testing"

func TestWrapPython_SkipsNonPython(t *testing.T) {
	cmd, args := wrapPython("npx", []string{"-y", "@x/server"})
	if cmd != "npx" || len(args) != 2 {
		t.Fatalf("npx must not be wrapped: %s %v", cmd, args)
	}
}

func TestWrapPython_Idempotent(t *testing.T) {
	cmd, args := wrapPython("python", []string{"/x/sdk_fix_wrapper.py", "server.py"})
	if cmd != "python" || args[0] != "/x/sdk_fix_wrapper.py" {
		t.Fatalf("already-wrapped command must be untouched: %s %v", cmd, args)
	}
}

func TestIsPythonCommand(t *testing.T) {
	cases := map[string]bool{"python": true, "python3": true, "python3.11": true, "node": false, "npx": false, "uvx": false}
	for in, want := range cases {
		if got := isPythonCommand(in); got != want {
			t.Errorf("isPythonCommand(%q) = %v, want %v", in, got, want)
		}
	}
}
