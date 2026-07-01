package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/appmgr"
	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/core/servicebus"
	"github.com/digitornai/digitorn/internal/modules/database"
	"github.com/digitornai/digitorn/internal/modulesettings"
	"github.com/digitornai/digitorn/internal/persistence/models"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/dispatch"
	"github.com/digitornai/digitorn/internal/server/mcpoauth"
)

type fakeAppGetter struct {
	byok bool
	cfg  map[string]any
}

func (f *fakeAppGetter) Get(_ context.Context, appID string) (*appmgr.RuntimeApp, error) {
	return &appmgr.RuntimeApp{
		Meta: &appmgr.App{AppID: appID, BYOK: f.byok},
		Definition: &schema.AppDefinition{
			Tools: &schema.ToolsBlock{
				Modules: map[string]schema.ModuleBlock{
					"database": {Config: f.cfg},
				},
			},
		},
	}, nil
}

func partsText(out runtime.ToolOutcome) string {
	var b strings.Builder
	for _, p := range out.Parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// TestModuleConfigInjection_LivePostgres drives the REAL dispatch seam end to
// end against a live Postgres: BusAdapter.Dispatch → appModuleConfigSource
// (BYOK-gated) → modulesettings deltas → WithModuleConfig → the database module
// opens the configured DSN and runs the query. It proves the per-user BYOK
// config delta actually changes which database the module talks to.
//
// Opt-in: set DIGITORN_PG_E2E_DSN to a reachable Postgres DSN whose `customers`
// table has rows for "Umbrella" and "Acme Corp" (see the test setup SQL).
func TestModuleConfigInjection_LivePostgres(t *testing.T) {
	realDSN := os.Getenv("DIGITORN_PG_E2E_DSN")
	if realDSN == "" {
		t.Skip("set DIGITORN_PG_E2E_DSN to run the live postgres injection test")
	}
	wrongDSN := os.Getenv("DIGITORN_PG_E2E_WRONG_DSN")
	if wrongDSN == "" {
		wrongDSN = "postgres://paul@/digitorn_nonexistent_zzz?host=/var/run/postgresql"
	}

	gdb, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("db: %v", err)
	}
	if err := gdb.AutoMigrate(&models.UserModuleConfig{}); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	sealer, err := mcpoauth.NewSealer(filepath.Join(t.TempDir(), "key"))
	if err != nil {
		t.Fatalf("sealer: %v", err)
	}
	store := modulesettings.NewStore(gdb, sealer)

	conn := func(dsn string) map[string]any {
		return map[string]any{
			"name": "prod", "kind": "postgres", "dsn": dsn,
			"security": map[string]any{"mode": "read_only", "max_rows": float64(100)},
		}
	}
	yamlCfg := map[string]any{
		"allow_raw_dsn": false,
		"databases":     []any{conn(wrongDSN)},
	}
	delta := map[string]any{"databases": []any{conn(realDSN)}}

	if err := store.Set(context.Background(), "tester", "db-test", "database", delta); err != nil {
		t.Fatalf("set delta: %v", err)
	}

	bus := servicebus.New()
	if err := bus.Register(database.New()); err != nil {
		t.Fatalf("register: %v", err)
	}

	query := func(byok bool, user string) runtime.ToolOutcome {
		a := &dispatch.BusAdapter{
			Bus:           bus,
			ModuleConfigs: appModuleConfigSource{apps: &fakeAppGetter{byok: byok, cfg: yamlCfg}, deltas: store},
		}
		return a.Dispatch(context.Background(), runtime.ToolInvocation{
			Name:   "database.query",
			AppID:  "db-test",
			UserID: user,
			Args: map[string]any{
				"query":      "SELECT name, plan, mrr FROM customers ORDER BY mrr DESC LIMIT 2",
				"connection": "prod",
			},
		})
	}

	t.Run("BYOK on + user delta hits the real db", func(t *testing.T) {
		out := query(true, "tester")
		txt := partsText(out)
		if out.Status != "completed" {
			t.Fatalf("status=%s err=%s parts=%s", out.Status, out.Error, txt)
		}
		if !strings.Contains(txt, "Umbrella") || !strings.Contains(txt, "Acme Corp") {
			t.Fatalf("expected real rows (Umbrella, Acme Corp), got: %s", txt)
		}
	})

	t.Run("BYOK off falls back to YAML default (wrong dsn) and errors", func(t *testing.T) {
		out := query(false, "tester")
		if out.Status != "errored" {
			t.Fatalf("expected errored on wrong default dsn, got status=%s parts=%s", out.Status, partsText(out))
		}
	})

	t.Run("BYOK on but other user has no delta → YAML default → errors", func(t *testing.T) {
		out := query(true, "other")
		if out.Status != "errored" {
			t.Fatalf("expected errored (no delta → default), got status=%s parts=%s", out.Status, partsText(out))
		}
	})
}
