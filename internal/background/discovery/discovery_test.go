package discovery

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/background/processor"
	"github.com/mbathepaul/digitorn/internal/background/store"
)

const channelApp = `
app:
  app_id: support
tools:
  modules:
    filesystem:
      config:
        workspace: "."
    channels:
      config:
        default_agent: main
        secret_filter_enabled: true
        providers:
          gh_hook:
            adapter: webhook
            config:
              inbound_path: /hook/gh
              auth: signature
              signature_secret: "{{secret.GH}}"
            activation:
              message: "PR {{event.payload.number}}"
              reply: none
          nightly:
            adapter: cron
            config:
              schedule: "0 9 * * *"
            activation:
              message: "run nightly"
          bad_sched:
            adapter: cron
            config:
              schedule: "not a cron"
            activation: { message: x }
          disabled_one:
            adapter: webhook
            enabled: false
            config: { inbound_path: /off }
            activation: { message: x }
`

const plainApp = `
app:
  app_id: chat
tools:
  modules:
    filesystem:
      config: { workspace: "." }
`

func writeApp(t *testing.T, dir, id, yaml string) {
	t.Helper()
	d := filepath.Join(dir, id)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "app.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScanApps(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "support", channelApp)
	writeApp(t, dir, "chat", plainApp)         // no channels → skipped
	writeApp(t, dir, "broken", "{{{ not yaml") // malformed → skipped

	apps, err := ScanApps(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].AppID != "support" {
		t.Fatalf("scan should find only the channel app, got %+v", apps)
	}
	if len(apps[0].Config.Providers) != 4 {
		t.Fatalf("providers = %d, want 4", len(apps[0].Config.Providers))
	}
}

func TestBuildPlan(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "support", channelApp)
	apps, _ := ScanApps(dir)

	env := func(k string) string {
		if k == "DIGITORN_BG_SECRET_GH" {
			return "topsecret"
		}
		return ""
	}
	plan := BuildPlan(apps, env)

	// disabled provider excluded; bad_sched dropped (warned); → 3 active triggers.
	if len(plan.Triggers) != 3 {
		t.Fatalf("triggers = %d, want 3 (gh_hook, nightly, bad_sched), got %+v", len(plan.Triggers), plan.Triggers)
	}
	if len(plan.Webhooks) != 1 {
		t.Fatalf("webhooks = %d, want 1", len(plan.Webhooks))
	}
	wh := plan.Webhooks[0]
	if wh.Path != "/hook/gh" || wh.Auth != "signature" {
		t.Fatalf("webhook provider wrong: %+v", wh)
	}
	if wh.Secret != "topsecret" {
		t.Fatalf("{{secret.GH}} not resolved, got %q", wh.Secret)
	}
	if len(plan.Crons) != 1 || plan.Crons[0].Name != "nightly" {
		t.Fatalf("crons = %+v, want [nightly] (bad_sched dropped)", plan.Crons)
	}
	// bad cron schedule produced a warning, not a crash.
	if len(plan.Warnings) == 0 {
		t.Fatal("expected a warning for the bad cron schedule")
	}
}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "bg.db") + "?_pragma=busy_timeout(5000)"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := gdb.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	st := store.New(gdb)
	if err := st.Migrate(); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestArm_PersistsTriggers_Idempotent(t *testing.T) {
	dir := t.TempDir()
	writeApp(t, dir, "support", channelApp)
	apps, _ := ScanApps(dir)
	plan := BuildPlan(apps, func(string) string { return "" })
	st := newStore(t)

	_, reg, err := Arm(context.Background(), st, plan, nil)
	if err != nil {
		t.Fatalf("Arm: %v", err)
	}
	if reg.Get("webhook") == nil || reg.Get("cron") == nil {
		t.Fatal("both webhook + cron adapters should be registered")
	}
	triggers, _ := st.ListTriggers(context.Background(), "")
	if len(triggers) != 3 {
		t.Fatalf("persisted triggers = %d, want 3", len(triggers))
	}

	// Re-arm (simulating a re-scan) must NOT duplicate (stable ids).
	if _, _, err := Arm(context.Background(), st, plan, nil); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	triggers2, _ := st.ListTriggers(context.Background(), "")
	if len(triggers2) != 3 {
		t.Fatalf("re-arm duplicated triggers: %d", len(triggers2))
	}
	// Stable id matches the expected derivation.
	want := processor.TriggerID("support", "gh_hook")
	found := false
	for _, tr := range triggers2 {
		if tr.ID == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected stable trigger id %q not found", want)
	}
}

func TestCfgStr_ResolvesPlaceholders(t *testing.T) {
	env := func(k string) string {
		return map[string]string{"DIGITORN_BG_SECRET_TOKEN": "s3cr3t", "WEBHOOK_PORT": "9000"}[k]
	}
	cfg := map[string]any{
		"a": "{{secret.TOKEN}}",
		"b": "port-{{env.WEBHOOK_PORT}}",
		"c": "plain",
	}
	if got := cfgStr(cfg, "a", env); got != "s3cr3t" {
		t.Fatalf("secret = %q", got)
	}
	if got := cfgStr(cfg, "b", env); got != "port-9000" {
		t.Fatalf("env = %q", got)
	}
	if got := cfgStr(cfg, "c", env); got != "plain" {
		t.Fatalf("plain = %q", got)
	}
}
