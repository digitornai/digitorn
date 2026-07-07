package processor

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/digitornai/digitorn/internal/background/store"
)

func newCfgStore(t *testing.T) *store.Store {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "m.db")), &gorm.Config{Logger: gormlogger.Default.LogMode(gormlogger.Silent)})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s := store.New(db)
	if err := s.Migrate(); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return s
}

func storedConfig(t *testing.T, s *store.Store, id string) map[string]any {
	t.Helper()
	tr, err := s.GetTrigger(context.Background(), id)
	if err != nil {
		t.Fatalf("get trigger: %v", err)
	}
	var spec TriggerSpec
	if err := json.Unmarshal([]byte(tr.ConfigJSON), &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return spec.Config
}

func TestReArmPreservesResolvedSecrets(t *testing.T) {
	ctx := context.Background()
	s := newCfgStore(t)
	m := NewManager(s, nil)

	pushed := TriggerSpec{AppID: "glpi-desk", Provider: "glpi", Adapter: "webhook",
		Config: map[string]any{"inbound_path": "/hook/glpi", "auth": "api_key", "api_key": "real-key-42"}}
	id, err := m.Arm(ctx, pushed)
	if err != nil {
		t.Fatalf("push arm: %v", err)
	}

	yamlScan := TriggerSpec{AppID: "glpi-desk", Provider: "glpi", Adapter: "webhook",
		Config: map[string]any{"inbound_path": "/hook/glpi", "auth": "api_key", "api_key": ""}}
	if _, err := m.Arm(ctx, yamlScan); err != nil {
		t.Fatalf("yaml re-arm: %v", err)
	}

	cfg := storedConfig(t, s, id)
	if cfg["api_key"] != "real-key-42" {
		t.Errorf("api_key = %v, want real-key-42 preserved across a blank re-arm", cfg["api_key"])
	}

	rotated := TriggerSpec{AppID: "glpi-desk", Provider: "glpi", Adapter: "webhook",
		Config: map[string]any{"inbound_path": "/hook/glpi", "auth": "api_key", "api_key": "new-key"}}
	if _, err := m.Arm(ctx, rotated); err != nil {
		t.Fatalf("rotate arm: %v", err)
	}
	if cfg := storedConfig(t, s, id); cfg["api_key"] != "new-key" {
		t.Errorf("api_key = %v, want new-key (a real value must win)", cfg["api_key"])
	}
}
