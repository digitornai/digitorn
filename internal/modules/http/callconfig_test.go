package http

import (
	"context"
	"testing"

	"github.com/digitornai/digitorn/pkg/module"
)

func TestCallConfigOverlaysAppConfig(t *testing.T) {
	m := New()
	if err := m.Init(context.Background(), map[string]any{}); err != nil {
		t.Fatalf("init: %v", err)
	}

	ctx := module.WithModuleConfig(context.Background(), map[string]any{
		"allow_private_hosts": true,
		"timeout":             5,
		"default_headers":     map[string]any{"App-Token": "tok"},
		"blocked_hosts":       []any{"evil.example"},
	})
	cfg := m.callConfig(ctx)

	if !cfg.AllowPrivateHosts {
		t.Error("app allow_private_hosts=true must be honored per call")
	}
	if cfg.TimeoutSecs != 5 {
		t.Errorf("app timeout must be honored, got %v", cfg.TimeoutSecs)
	}
	if cfg.DefaultHeaders["App-Token"] != "tok" {
		t.Error("app default_headers must be merged")
	}
	if err := m.checkHost(cfg, "https://evil.example/x"); err == nil {
		t.Error("app blocked_hosts must extend the blocklist")
	}
}

func TestCallConfigNoAppBlockKeepsBootConfig(t *testing.T) {
	m := New()
	if err := m.Init(context.Background(), map[string]any{"allow_private_hosts": true}); err != nil {
		t.Fatalf("init: %v", err)
	}
	cfg := m.callConfig(context.Background())
	if !cfg.AllowPrivateHosts {
		t.Error("boot config must pass through when no app block is attached")
	}
}

func TestAppCannotUnblockDaemonBlockedHost(t *testing.T) {
	m := New()
	if err := m.Init(context.Background(), map[string]any{"blocked_hosts": []any{"internal.corp"}}); err != nil {
		t.Fatalf("init: %v", err)
	}
	ctx := module.WithModuleConfig(context.Background(), map[string]any{
		"blocked_hosts": []any{},
	})
	cfg := m.callConfig(ctx)
	if err := m.checkHost(cfg, "https://internal.corp/x"); err == nil {
		t.Error("daemon blocklist must survive an app override")
	}
}
