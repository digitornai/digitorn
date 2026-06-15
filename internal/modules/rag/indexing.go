package rag

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/indexer"
)

// cursorStore returns the most durable cursor available, in order : a shared
// Postgres store (set DIGITORN_INDEX_CURSOR_DSN — survives restarts AND is
// distributed, with cluster-wide per-source leases so replicas never double-
// index), else a per-host file cursor under ~/.digitorn, else in-memory.
func cursorStore() indexer.Cursor {
	if dsn := strings.TrimSpace(os.Getenv("DIGITORN_INDEX_CURSOR_DSN")); dsn != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if st, err := indexer.NewPgStore(ctx, dsn); err == nil {
			return st
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		if c, err := indexer.NewFileCursor(filepath.Join(home, ".digitorn", "index-cursors")); err == nil {
			return c
		}
	}
	return nil
}

// cursorDSNFor places an app's indexer sync-state in its OWN database : an
// explicit cursor_dsn wins ; otherwise, when the vector backend is pgvector,
// reuse that DSN so index + state live in one client-owned Postgres (nothing
// local). Empty → the service's default cursor.
func cursorDSNFor(cfg Config) string {
	if d := strings.TrimSpace(cfg.CursorDSN); d != "" {
		return d
	}
	if isDatabaseSource(cfg.Backend.Type) {
		if d := strings.TrimSpace(cfg.Backend.DSN); d != "" {
			return d
		}
		if d := strings.TrimSpace(cfg.Backend.URL); d != "" {
			return d
		}
	}
	return ""
}

// appKBs returns the distinct knowledge bases the app's sources target.
func appKBs(cfg Config) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(cfg.Sources))
	for _, s := range cfg.Sources {
		kb := s.KnowledgeBase
		if kb == "" {
			kb = "default"
		}
		if !seen[kb] {
			seen[kb] = true
			out = append(out, kb)
		}
	}
	return out
}

// resolveKBs routes a query to the right knowledge base(s) from the app
// context : an explicit name wins ; else the configured default ; else every
// KB the app's sources declare (so the agent never has to name an index).
func resolveKBs(cfg Config, requested string) []string {
	if r := strings.TrimSpace(requested); r != "" {
		return []string{r}
	}
	if d := strings.TrimSpace(cfg.DefaultKnowledgeBase); d != "" {
		return []string{d}
	}
	if kbs := appKBs(cfg); len(kbs) > 0 {
		return kbs
	}
	return []string{"default"}
}

// ragSink adapts the rag Engine to indexer.Sink : each indexed document is
// clean-replaced (delete-by-source then chunk+embed+store) keyed by its
// source id. ACL/cache/migrate live downstream in the engine, unchanged.
type ragSink struct{ eng *Engine }

func (s ragSink) Upsert(ctx context.Context, kb string, docs []indexer.Document) error {
	// Clean-replace each source's previous chunks (handles updates), in parallel
	// so the round-trips don't serialize on a large batch.
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, d := range docs {
		wg.Add(1)
		sem <- struct{}{}
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			_ = s.eng.backend.DeleteBySource(ctx, kb, id)
		}(d.ID)
	}
	wg.Wait()

	items := make([]SourceDoc, len(docs))
	for i, d := range docs {
		items[i] = SourceDoc{ID: d.ID, Text: d.Text, Source: d.ID, Meta: d.Meta}
	}
	_, err := s.eng.IngestBatch(ctx, kb, items)
	return err
}

func (s ragSink) Delete(ctx context.Context, kb, id string) error {
	return s.eng.backend.DeleteBySource(ctx, kb, id)
}

func isWebSource(t string) bool {
	switch strings.ToLower(t) {
	case "web", "url", "website":
		return true
	}
	return false
}

func isKafkaSource(t string) bool { return strings.ToLower(t) == "kafka" }

func isCodebaseSource(t string) bool {
	switch strings.ToLower(t) {
	case "codebase", "code", "repo", "repository":
		return true
	}
	return false
}

// specFor maps any SourceConfig to its indexer SourceSpec. Used both to
// register sources (engineFor) and to invoke a manual re-index (reindex tool).
func specFor(src SourceConfig, auto AutoIndex) (indexer.SourceSpec, bool) {
	switch {
	case isWebSource(src.Type):
		return webSpec(src, auto), true
	case isKafkaSource(src.Type):
		return kafkaSpec(src, auto), true
	case isGenericDBSource(src.Type):
		return genericDBSpec(src, auto), true
	case isDatabaseSource(src.Type):
		return dbSpec(src, auto), true
	case isCodebaseSource(src.Type):
		return codebaseSpec(src, auto), true
	case isFileSource(src.Type):
		return fileSpec(src, auto), true
	}
	return indexer.SourceSpec{}, false
}

func codebaseSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = src.Path
	}
	opts := map[string]any{"path": src.Path}
	if src.SymbolChunks != nil {
		opts["symbol_chunks"] = *src.SymbolChunks
	}
	return indexer.SourceSpec{Name: name, Type: "codebase", KB: kb, Opts: opts, Triggers: triggersFor(src, auto)}
}

func kafkaSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = src.Topic
	}
	opts := map[string]any{"brokers": src.Brokers, "topic": src.Topic}
	if src.GroupID != "" {
		opts["group_id"] = src.GroupID
	}
	if src.IDField != "" {
		opts["id_field"] = src.IDField
	}
	if len(src.TextFields) > 0 {
		opts["text_fields"] = src.TextFields
	}
	trigs := triggersFor(src, auto)
	if len(trigs) == 0 {
		trigs = []indexer.Trigger{{Type: "watch"}} // a stream is watched by default
	}
	return indexer.SourceSpec{Name: name, Type: "kafka", KB: kb, Opts: opts, Triggers: trigs}
}

func isFileSource(t string) bool {
	switch strings.ToLower(t) {
	case "", "file", "dir", "directory":
		return true
	}
	return false
}

func isDatabaseSource(t string) bool {
	switch strings.ToLower(t) {
	case "database", "postgres", "postgresql", "pg":
		return true
	}
	return false
}

// isGenericDBSource matches every database the shared dbaccess socle can Walk
// (any SQL dialect + MongoDB) — distinct from the Postgres "database" source,
// which keeps its dedicated CDC connector.
func isGenericDBSource(t string) bool {
	switch strings.ToLower(t) {
	case "mysql", "mariadb", "sql", "sqlite", "sqlserver", "mssql", "mongodb", "mongo":
		return true
	}
	return false
}

// genericDBSpec maps a SourceConfig to the dbaccess-backed indexer connector :
// the configured query's rows become documents (id_column → id, text_columns →
// text). Works for any SQL dialect and MongoDB (query = its native form).
func genericDBSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = src.Type
	}
	opts := map[string]any{"dsn": src.DSN, "query": src.Query, "id_column": src.IDColumn}
	if len(src.TextColumns) > 0 {
		opts["text_columns"] = src.TextColumns
	}
	return indexer.SourceSpec{Name: name, Type: strings.ToLower(src.Type), KB: kb, Opts: opts, Triggers: triggersFor(src, auto)}
}

// dbSpec maps a SourceConfig to an indexer SourceSpec for the database
// connector (SQL query → rows ; CDC real-time via triggers).
func dbSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = "db"
	}
	opts := map[string]any{"dsn": src.DSN, "query": src.Query, "id_column": src.IDColumn}
	if len(src.TextColumns) > 0 {
		opts["text_columns"] = src.TextColumns
	}
	if src.CDCTable != "" {
		opts["cdc_table"] = src.CDCTable
	}
	if src.CDCSlot != "" {
		opts["cdc_slot"] = src.CDCSlot
	}
	if src.CDCPublication != "" {
		opts["cdc_publication"] = src.CDCPublication
	}
	return indexer.SourceSpec{Name: name, Type: "database", KB: kb, Opts: opts, Triggers: triggersFor(src, auto)}
}

// fileSpec maps a SourceConfig to an indexer SourceSpec for the file/document
// connector (Tabula-backed extraction).
func fileSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = src.Path
	}
	opts := map[string]any{"path": src.Path}
	if src.Recursive != nil {
		opts["recursive"] = *src.Recursive
	}
	if src.MaxFiles > 0 {
		opts["max_files"] = src.MaxFiles
	}
	if len(src.Extensions) > 0 {
		opts["extensions"] = src.Extensions
	}
	return indexer.SourceSpec{Name: name, Type: "file", KB: kb, Opts: opts, Triggers: triggersFor(src, auto)}
}

// webSpec maps a SourceConfig to an indexer SourceSpec for the web connector,
// only setting options the app actually provided (so connector defaults
// apply otherwise).
func webSpec(src SourceConfig, auto AutoIndex) indexer.SourceSpec {
	kb := src.KnowledgeBase
	if kb == "" {
		kb = "default"
	}
	name := src.Name
	if name == "" {
		name = src.URL
	}
	opts := map[string]any{"url": src.URL}
	if src.MaxPages > 0 {
		opts["max_pages"] = src.MaxPages
	}
	if src.MaxDepth > 0 {
		opts["max_depth"] = src.MaxDepth
	}
	if src.Parallelism > 0 {
		opts["parallelism"] = src.Parallelism
	}
	if src.RateLimit != "" {
		opts["rate_limit"] = src.RateLimit
	}
	if len(src.Include) > 0 {
		opts["include"] = src.Include
	}
	if len(src.Exclude) > 0 {
		opts["exclude"] = src.Exclude
	}
	if src.SameDomain != nil {
		opts["same_domain"] = *src.SameDomain
	}
	if src.Sitemap != nil {
		opts["sitemap"] = *src.Sitemap
	}
	if src.RespectRobots != nil {
		opts["respect_robots"] = *src.RespectRobots
	}
	if src.AllowPrivate != nil {
		opts["allow_private"] = *src.AllowPrivate
	}
	return indexer.SourceSpec{Name: name, Type: "web", KB: kb, Opts: opts, Triggers: triggersFor(src, auto)}
}

// triggersFor resolves a source's triggers : explicit per-source triggers
// win ; otherwise it falls back to the app-global auto_index (backward
// compat — on_start + schedule as interval-or-cron).
func triggersFor(src SourceConfig, auto AutoIndex) []indexer.Trigger {
	if len(src.Triggers) > 0 {
		out := make([]indexer.Trigger, 0, len(src.Triggers))
		for _, t := range src.Triggers {
			it := indexer.Trigger{Type: t.Type, Cron: t.Cron}
			if d, err := time.ParseDuration(strings.TrimSpace(t.Every)); err == nil {
				it.Every = d
			}
			out = append(out, it)
		}
		return out
	}
	var out []indexer.Trigger
	if auto.OnStart {
		out = append(out, indexer.Trigger{Type: "on_start"})
	}
	if s := strings.TrimSpace(auto.Schedule); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			out = append(out, indexer.Trigger{Type: "interval", Every: d})
		} else {
			out = append(out, indexer.Trigger{Type: "cron", Cron: s})
		}
	}
	return out
}
