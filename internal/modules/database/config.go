package database

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/dbaccess"
)

// Config is the per-app database module configuration : a set of named
// connections (opened + pooled by the service, so the agent usually only needs
// `query`) plus the gate for agent-supplied raw DSNs.
type Config struct {
	Databases   []DBConn `json:"databases"`
	AllowRawDSN bool     `json:"allow_raw_dsn"`
}

type DBConn struct {
	Name         string         `json:"name"`
	Kind         string         `json:"kind"`
	DSN          string         `json:"dsn"`     // direct (less safe)
	DSNRef       string         `json:"dsn_ref"` // env var name → creds stay server-side
	SampleValues bool           `json:"sample_values"`
	Security     SecurityConfig `json:"security"`
	Schema       SchemaConfig   `json:"schema"`
}

type SecurityConfig struct {
	Mode              string   `json:"mode"`
	EnforceDBReadOnly bool     `json:"enforce_db_readonly"`
	ApplyRole         string   `json:"apply_role"`
	StatementTimeout  string   `json:"statement_timeout"`
	DeniedStatements  []string `json:"denied_statements"`
	AllowedTables     []string `json:"allowed_tables"`
	MaxRows           int      `json:"max_rows"`
	SensitiveColumns  []string `json:"sensitive_columns"`
	Egress            string   `json:"egress"`
}

type SchemaConfig struct {
	Tables map[string]TableConfig `json:"tables"`
}

type TableConfig struct {
	Description string            `json:"description"`
	Aka         []string          `json:"aka"`
	Columns     map[string]string `json:"columns"`
	Relations   []string          `json:"relations"`
	Golden      []GoldenConfig    `json:"golden_queries"`
	Sensitive   []string          `json:"sensitive"`
}

type GoldenConfig struct {
	Q   string `json:"q"`
	SQL string `json:"sql"`
}

func ParseConfig(raw map[string]any) (Config, error) {
	var c Config
	if len(raw) == 0 {
		return c, nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return Config{}, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return Config{}, fmt.Errorf("database config: %w", err)
	}
	return c, nil
}

func (c Config) find(name string) (DBConn, bool) {
	for _, d := range c.Databases {
		if strings.EqualFold(d.Name, name) {
			return d, true
		}
	}
	return DBConn{}, false
}

// toConnConfig resolves the DSN (dsn_ref → env, never exposing creds to the
// agent) and maps the YAML security + schema decoration onto the dbaccess types.
func (d DBConn) toConnConfig() (dbaccess.ConnConfig, error) {
	dsn := strings.TrimSpace(d.DSN)
	if d.DSNRef != "" {
		if v := strings.TrimSpace(os.Getenv(d.DSNRef)); v != "" {
			dsn = v
		}
	}
	if dsn == "" {
		return dbaccess.ConnConfig{}, fmt.Errorf("database %q: no dsn (set dsn or dsn_ref)", d.Name)
	}

	pol := dbaccess.SecurityPolicy{
		Mode:              d.Security.Mode,
		EnforceDBReadOnly: d.Security.EnforceDBReadOnly,
		ApplyRole:         d.Security.ApplyRole,
		DeniedStatements:  d.Security.DeniedStatements,
		AllowedTables:     d.Security.AllowedTables,
		MaxRows:           d.Security.MaxRows,
		SensitiveColumns:  d.Security.SensitiveColumns,
		Egress:            d.Security.Egress,
	}
	if s := strings.TrimSpace(d.Security.StatementTimeout); s != "" {
		if dur, err := time.ParseDuration(s); err == nil {
			pol.StatementTimeout = dur
		}
	}

	decor := dbaccess.SchemaDecor{Tables: map[string]dbaccess.TableDecor{}}
	for name, tc := range d.Schema.Tables {
		td := dbaccess.TableDecor{
			Description: tc.Description,
			Aka:         tc.Aka,
			Columns:     tc.Columns,
			Relations:   tc.Relations,
			Sensitive:   tc.Sensitive,
		}
		for _, g := range tc.Golden {
			td.Golden = append(td.Golden, dbaccess.GoldenQuery{Q: g.Q, SQL: g.SQL})
		}
		decor.Tables[name] = td
	}

	return dbaccess.ConnConfig{
		Name: d.Name, Kind: d.Kind, DSN: dsn,
		SampleValues: d.SampleValues, Security: pol, Decor: decor,
	}, nil
}
