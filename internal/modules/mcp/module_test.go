package mcp

import (
	"context"
	"testing"
)

func TestParseVirtual(t *testing.T) {
	cases := []struct {
		name, server, action string
		ok                   bool
	}{
		{"mcp_github__create_issue", "github", "create_issue", true},
		{"mcp_google_calendar__list_events", "google_calendar", "list_events", true},
		{"filesystem__read", "", "", false},
		{"mcp_x", "", "", false},
		{"read", "", "", false},
	}
	for _, c := range cases {
		s, a, ok := parseVirtual(c.name)
		if ok != c.ok || s != c.server || a != c.action {
			t.Errorf("%s → (%q,%q,%v), want (%q,%q,%v)", c.name, s, a, ok, c.server, c.action, c.ok)
		}
	}
}

func TestModuleInitConnectsAndInvokes(t *testing.T) {
	fc := &fakeConn{tools: toolList("echo")}
	m := New()
	m.pool.dialFn = func(context.Context, connectSpec) (mcpConn, error) { return fc, nil }

	cfg := map[string]any{"servers": map[string]any{
		"srv": map[string]any{
			"transport": "stdio", "command": "x",
			"sandbox": map[string]any{"permissions": []any{"process.exec"}},
		},
	}}
	if err := m.Init(context.Background(), cfg); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, ok := m.pool.get("srv"); !ok {
		t.Fatal("server not connected by Init")
	}

	res, err := m.Invoke(context.Background(), "mcp_srv__echo", []byte(`{"x":1}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !res.Success {
		t.Fatalf("expected success, got %+v", res)
	}
	data, ok := res.Data.(map[string]any)
	if !ok {
		t.Fatalf("Data not a map: %T", res.Data)
	}
	if data["_source"] != "mcp_server:srv" {
		t.Errorf("source = %v", data["_source"])
	}
	if data["_note"] != injectionNote {
		t.Error("missing injection note (prompt-injection defense)")
	}
	if data["output"] != "ok:echo" {
		t.Errorf("output = %v", data["output"])
	}
}

func TestModuleInvokeUnroutable(t *testing.T) {
	m := New()
	if _, err := m.Invoke(context.Background(), "not_mcp_tool", nil); err == nil {
		t.Fatal("expected error for an unroutable tool name")
	}
}
