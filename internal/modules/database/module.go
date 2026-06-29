// Package database is the agent-facing universal database module : three
// tools — connect / disconnect / query — over the shared dbaccess socle, so an
// agent can read (and, when allowed, write) ANY database (SQL or NoSQL) behind
// a configurable, layered security policy. Connections are pooled by the
// service (worker-hosted), so the daemon is never blocked, and config-declared
// connections are usable by `query` without ever calling `connect`.
package database

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/dbaccess"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/pkg/module"
)

type Module struct {
	module.Base
	mgr *dbaccess.Manager
}

func New() *Module {
	m := &Module{mgr: dbaccess.NewManager(256, 30*time.Minute)}
	m.Base = module.Base{
		ID:          "database",
		Version:     "1.0.0",
		Description: "Universal database access — connect to and query any SQL or NoSQL database behind a configurable security policy, with decorated schema context.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux, domainmodule.PlatformMacOS, domainmodule.PlatformWindows,
		},
		ConfigSchema: module.SchemaFromType(&Config{}),
	}

	m.RegisterTool(module.Tool{
		Name:        "connect",
		Description: "Open (or reuse) a database connection and return its decorated schema (tables, columns, relationships, business meaning). Pass a configured connection by `name`, or — if the app allows raw DSNs — `kind`+`dsn`. Often unnecessary: configured databases are usable directly via `query`.",
		Params: []tool.ParamSpec{
			{Name: "name", Type: "string", Description: "Name of a database declared in the app config."},
			{Name: "kind", Type: "string", Description: "Engine for a raw connection: postgres|mysql|… (requires allow_raw_dsn)."},
			{Name: "dsn", Type: "string", Description: "Raw DSN for an ad-hoc connection (requires allow_raw_dsn)."},
		},
		RiskLevel: tool.RiskHigh, Tags: []string{"database"}, CLILabel: "DB connect", CLIParam: "name",
		Handler: m.connect,
	})
	m.RegisterTool(module.Tool{
		Name:        "query",
		Description: "Run a query against a connection and return the rows. Write the engine's native language (SQL for SQL databases). The security policy may enforce read-only, row caps, timeouts and PII masking. `connection` defaults to the sole configured database when there is only one.",
		Params: []tool.ParamSpec{
			{Name: "query", Type: "string", Description: "The query to run (e.g. a SELECT statement).", Required: true},
			{Name: "connection", Type: "string", Description: "Connection name or id. Optional when the app has a single configured database."},
		},
		RiskLevel: tool.RiskMedium, Tags: []string{"database"}, CLILabel: "DB query", CLIParam: "query",
		Handler: m.query,
	})
	m.RegisterTool(module.Tool{
		Name:        "disconnect",
		Description: "Close a database connection by name or id.",
		Params:      []tool.ParamSpec{{Name: "connection", Type: "string", Description: "Connection name or id to close.", Required: true}},
		RiskLevel:   tool.RiskLow, Tags: []string{"database"}, CLILabel: "DB disconnect", CLIParam: "connection",
		Handler: m.disconnect,
	})

	return m
}

// Stop closes every pooled connection on module teardown.
func (m *Module) Stop(ctx context.Context) error {
	m.mgr.Shutdown()
	return m.Base.Stop(ctx)
}

func (m *Module) connect(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Name   string `json:"name"`
		Kind   string `json:"kind"`
		DSN    string `json:"dsn"`
		DSNRef string `json:"dsn_ref"`
	}
	_ = json.Unmarshal(raw, &p)
	cfg, _ := ParseConfig(module.ModuleConfigFrom(ctx))
	app := module.AppID(ctx)

	if strings.TrimSpace(p.Name) != "" {
		d, ok := cfg.find(p.Name)
		if !ok {
			return fail(fmt.Sprintf("no configured database named %q (have: %s)", p.Name, names(cfg))), nil
		}
		cc, err := d.toConnConfig()
		if err != nil {
			return fail(err.Error()), nil
		}
		db, err := m.mgr.Named(ctx, app, cc)
		if err != nil {
			return fail(err.Error()), nil
		}
		cat, _ := db.Schema(ctx)
		return okData(map[string]any{"connection": p.Name, "kind": cc.Kind, "schema": cat}), nil
	}

	if !cfg.AllowRawDSN {
		return fail("raw DSN connections are disabled (allow_raw_dsn=false); connect by configured name"), nil
	}
	dsn := strings.TrimSpace(p.DSN)
	if p.DSNRef != "" {
		dsn = strings.TrimSpace(os.Getenv(p.DSNRef))
	}
	if strings.TrimSpace(p.Kind) == "" || dsn == "" {
		return fail("kind and dsn (or dsn_ref) are required for a raw connection"), nil
	}
	cc := dbaccess.ConnConfig{
		Kind: p.Kind, DSN: dsn, SampleValues: true,
		Security: dbaccess.SecurityPolicy{Mode: "read_only", Egress: "guarded"}, // safe default for ad-hoc
	}
	id, db, err := m.mgr.Open(ctx, app, cc)
	if err != nil {
		return fail(err.Error()), nil
	}
	cat, _ := db.Schema(ctx)
	return okData(map[string]any{"connection": id, "kind": p.Kind, "schema": cat}), nil
}

func (m *Module) query(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Connection string `json:"connection"`
		Query      string `json:"query"`
	}
	_ = json.Unmarshal(raw, &p)
	if strings.TrimSpace(p.Query) == "" {
		return fail("query is required"), nil
	}
	cfg, _ := ParseConfig(module.ModuleConfigFrom(ctx))
	app := module.AppID(ctx)
	db, err := m.resolve(ctx, app, cfg, p.Connection)
	if err != nil {
		return fail(err.Error()), nil
	}
	res, err := db.Query(ctx, p.Query)
	if err != nil {
		return fail(err.Error()), nil
	}
	return tool.Result{
		Success: true,
		Data:    map[string]any{"columns": res.Columns, "rows": res.Rows, "row_count": res.RowCount, "truncated": res.Truncated},
		Display: &tool.DisplayHint{Type: "json", Title: "DB query"},
	}, nil
}

func (m *Module) disconnect(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Connection string `json:"connection"`
	}
	_ = json.Unmarshal(raw, &p)
	if err := m.mgr.Close(module.AppID(ctx), p.Connection); err != nil {
		return fail(err.Error()), nil
	}
	return okData(map[string]any{"connection": p.Connection, "closed": true}), nil
}

func (m *Module) resolve(ctx context.Context, app string, cfg Config, conn string) (dbaccess.DB, error) {
	conn = strings.TrimSpace(conn)
	if conn == "" {
		if len(cfg.Databases) == 1 {
			conn = cfg.Databases[0].Name
		} else {
			return nil, fmt.Errorf("connection is required (configured: %s)", names(cfg))
		}
	}
	if d, ok := cfg.find(conn); ok {
		cc, err := d.toConnConfig()
		if err != nil {
			return nil, err
		}
		return m.mgr.Named(ctx, app, cc)
	}
	if db, ok := m.mgr.Get(app, conn); ok {
		return db, nil
	}
	return nil, fmt.Errorf("unknown connection %q", conn)
}

func names(cfg Config) string {
	out := make([]string, 0, len(cfg.Databases))
	for _, d := range cfg.Databases {
		out = append(out, d.Name)
	}
	return strings.Join(out, ", ")
}

func okData(data map[string]any) tool.Result { return tool.Result{Success: true, Data: data} }
func fail(msg string) tool.Result            { return tool.Result{Success: false, Error: msg} }
