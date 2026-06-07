package schema

import "testing"

func TestNormalizeServersBareList(t *testing.T) {
	out, bad := NormalizeServers([]any{"github", "notion"})
	if len(bad) != 0 {
		t.Fatalf("unexpected bad: %v", bad)
	}
	for _, id := range []string{"github", "notion"} {
		s, ok := out[id]
		if !ok {
			t.Fatalf("missing %q", id)
		}
		if s.Sandbox == nil || len(s.Sandbox.Permissions) != 2 {
			t.Fatalf("%q: bare ref must get default sandbox, got %+v", id, s.Sandbox)
		}
	}
}

func TestNormalizeServersEmptyRefGetsDefaultSandbox(t *testing.T) {
	out, bad := NormalizeServers(map[string]any{"memory": map[string]any{}})
	if len(bad) != 0 {
		t.Fatalf("unexpected bad: %v", bad)
	}
	if out["memory"].Sandbox == nil {
		t.Fatal("empty ref must get default sandbox")
	}
}

func TestNormalizeServersInline(t *testing.T) {
	out, bad := NormalizeServers(map[string]any{
		"custom": map[string]any{
			"transport": "stdio",
			"command":   "/bin/srv",
			"args":      []any{"--port", "auto"},
			"sandbox":   map[string]any{"permissions": []any{"process.exec", "fs.read"}},
		},
	})
	if len(bad) != 0 {
		t.Fatalf("unexpected bad: %v", bad)
	}
	s := out["custom"]
	if s.Transport != MCPTransportStdio || s.Command != "/bin/srv" {
		t.Fatalf("inline decode wrong: %+v", s)
	}
	if len(s.Args) != 2 || s.Sandbox == nil || len(s.Sandbox.Permissions) != 2 {
		t.Fatalf("inline args/sandbox wrong: %+v", s)
	}
}

func TestNormalizeServersShorthandToExtra(t *testing.T) {
	out, _ := NormalizeServers(map[string]any{"github": map[string]any{"token": "abc"}})
	s := out["github"]
	if s.Extra["token"] != "abc" {
		t.Fatalf("catalog shorthand must land in Extra, got %+v", s.Extra)
	}
}

func TestNormalizeServersMalformed(t *testing.T) {
	_, bad := NormalizeServers(map[string]any{"x": 123})
	if len(bad) != 1 || bad[0] != "x" {
		t.Fatalf("malformed entry must be reported, got %v", bad)
	}
}
