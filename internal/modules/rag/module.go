// Package rag implements per-app knowledge bases on the app's own vector
// server, with chunking, semantic search and citations. Worker-hosted :
// embeddings come from the daemon gateway, per-app config from the call ctx.
package rag

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/indexer"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// engEntry is one cached per-(app,config) engine plus the sources it
// registered (to deregister on eviction) and its last-use time (for LRU).
type engEntry struct {
	eng    *Engine
	specs  []indexer.SourceSpec
	usedAt time.Time
}

// engineTTL evicts an engine idle for this long (frees its backend
// connection + stops its source syncs). The hard size bound is maxEngines.
const engineTTL = 30 * time.Minute

type Module struct {
	module.Base
	mu         sync.Mutex
	engines    map[string]*engEntry // (app, config) -> engine, LRU+TTL bounded
	maxEngines int
	idx        *indexer.Service // shared indexation service (connectors + triggers)
}

func New() *Module {
	m := &Module{engines: map[string]*engEntry{}, maxEngines: 256, idx: indexer.NewService(cursorStore(), 8)}
	m.Base = module.Base{
		ID:          "rag",
		Version:     "1.0.0",
		Description: "Advanced RAG — per-app knowledge bases on the app's vector server, with chunking, semantic search and citations.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux, domainmodule.PlatformMacOS, domainmodule.PlatformWindows,
		},
	}

	m.RegisterTool(module.Tool{
		Name:        "create_knowledge_base",
		Description: "Create a knowledge base (a collection on the app's vector server) sized to the embedding model.",
		Params: []tool.ParamSpec{
			{Name: "name", Type: "string", Description: "Knowledge base name.", Required: true},
			{Name: "description", Type: "string", Description: "Optional human description."},
		},
		RiskLevel: tool.RiskLow, Tags: []string{"rag", "knowledge"}, CLILabel: "Create KB", CLIParam: "name",
		Handler: m.createKB,
	})
	m.RegisterTool(module.Tool{
		Name:        "list_knowledge_bases",
		Description: "List the knowledge bases on the app's vector server with their document counts.",
		RiskLevel:   tool.RiskLow, Tags: []string{"rag", "knowledge"}, CLILabel: "List KBs",
		Handler: m.listKBs,
	})
	m.RegisterTool(module.Tool{
		Name:        "knowledge_base_stats",
		Description: "Document count + details for one knowledge base.",
		Params:      []tool.ParamSpec{{Name: "name", Type: "string", Description: "Knowledge base name.", Required: true}},
		RiskLevel:   tool.RiskLow, Tags: []string{"rag", "knowledge"}, CLILabel: "KB stats", CLIParam: "name",
		Handler: m.kbStats,
	})
	m.RegisterTool(module.Tool{
		Name:        "delete_knowledge_base",
		Description: "Delete a knowledge base and all its vectors. Irreversible.",
		Params:      []tool.ParamSpec{{Name: "name", Type: "string", Description: "Knowledge base name.", Required: true}},
		RiskLevel:   tool.RiskHigh, Tags: []string{"rag", "knowledge"}, CLILabel: "Delete KB", CLIParam: "name",
		Handler: m.deleteKB,
	})
	m.RegisterTool(module.Tool{
		Name:        "ingest",
		Description: "Ingest raw text into a knowledge base : chunk, embed and store with citation metadata.",
		Params: []tool.ParamSpec{
			{Name: "knowledge_base", Type: "string", Description: "Target knowledge base.", Required: true},
			{Name: "text", Type: "string", Description: "Raw text to ingest.", Required: true},
			{Name: "source", Type: "string", Description: "Citation label for this text (title or filename)."},
			{Name: "metadata", Type: "object", Description: "Optional tags stored with each chunk and usable as retrieval filters (e.g. team, doc_type)."},
		},
		RiskLevel: tool.RiskLow, Tags: []string{"rag", "ingest"}, CLILabel: "Ingest", CLIParam: "knowledge_base",
		Handler: m.ingest,
	})
	m.RegisterTool(module.Tool{
		Name:        "ingest_file",
		Description: "Ingest one file into a knowledge base : extract text (txt/md/code/csv/json/html/pdf/docx), chunk, embed and store.",
		Params: []tool.ParamSpec{
			{Name: "knowledge_base", Type: "string", Description: "Target knowledge base.", Required: true},
			{Name: "path", Type: "string", Description: "File path to ingest.", Required: true},
			{Name: "source", Type: "string", Description: "Citation label (defaults to the filename)."},
		},
		RiskLevel: tool.RiskLow, Tags: []string{"rag", "ingest"}, CLILabel: "Ingest file", CLIParam: "path",
		Handler: m.ingestFile,
	})
	m.RegisterTool(module.Tool{
		Name:        "ingest_directory",
		Description: "Ingest every supported file in a directory (chunk, embed, store). Skips unsupported/binary files.",
		Params: []tool.ParamSpec{
			{Name: "knowledge_base", Type: "string", Description: "Target knowledge base.", Required: true},
			{Name: "path", Type: "string", Description: "Directory to ingest.", Required: true},
			{Name: "recursive", Type: "boolean", Description: "Recurse into sub-directories (default true).", Default: true},
			{Name: "extensions", Type: "string_list", Description: "Restrict to these extensions (e.g. .md,.pdf). Empty = all supported."},
			{Name: "max_files", Type: "integer", Description: "Cap on files ingested (default 1000)."},
		},
		RiskLevel: tool.RiskLow, Tags: []string{"rag", "ingest"}, CLILabel: "Ingest dir", CLIParam: "path",
		Handler: m.ingestDirectory,
	})
	m.RegisterTool(module.Tool{
		Name:        "reindex",
		Description: "Re-index all configured sources now through the indexation service (admin / control-plane). Returns per-source counts.",
		RiskLevel:   tool.RiskHigh, Tags: []string{"rag", "admin"}, CLILabel: "Reindex sources",
		Handler: m.reindex,
	})
	m.RegisterTool(module.Tool{
		Name:        "index_stats",
		Description: "Indexation service runtime metrics : syncs, docs upserted/deleted, dead-lettered, watch events/errors, lease skips, in-flight, jobs, watches.",
		RiskLevel:   tool.RiskLow, Tags: []string{"rag", "admin"}, CLILabel: "Index stats",
		Handler: m.indexStats,
	})
	m.RegisterTool(module.Tool{
		Name:        "migrate_embeddings",
		Description: "Re-embed a knowledge base with a different embedding model into a new knowledge base (preserves text, source, position and metadata).",
		Params: []tool.ParamSpec{
			{Name: "knowledge_base", Type: "string", Description: "Source knowledge base.", Required: true},
			{Name: "target", Type: "string", Description: "Target knowledge base (default: <source>__<model>)."},
			{Name: "model", Type: "string", Description: "New embedding model id (default: the configured model)."},
		},
		RiskLevel: tool.RiskMedium, Tags: []string{"rag", "admin"}, CLILabel: "Migrate embeddings", CLIParam: "knowledge_base",
		Handler: m.migrateEmbeddings,
	})
	m.RegisterTool(module.Tool{
		Name:        "query",
		Description: "Search a knowledge base and return the top chunks with citations.",
		Params: []tool.ParamSpec{
			{Name: "query", Type: "string", Description: "Natural-language question.", Required: true},
			{Name: "knowledge_base", Type: "string", Description: "Optional. Omit to let the engine route to the app's knowledge base(s) automatically."},
			{Name: "top_k", Type: "integer", Description: "Max chunks to return (default from config)."},
		},
		RiskLevel: tool.RiskLow, Tags: []string{"rag", "search"}, CLILabel: "RAG query", CLIParam: "query",
		Handler: m.query,
	})

	return m
}

// Stop drains the indexation service (waits for in-flight syncs, cancels watch
// streams) and closes every cached engine before the base teardown.
func (m *Module) Stop(ctx context.Context) error {
	m.idx.Shutdown(ctx)
	m.mu.Lock()
	for k, ent := range m.engines {
		_ = ent.eng.Close()
		delete(m.engines, k)
	}
	m.mu.Unlock()
	return m.Base.Stop(ctx)
}

func (m *Module) engineFor(ctx context.Context) (*Engine, error) {
	cfgMap := module.ModuleConfigFrom(ctx)
	cfg, err := ParseConfig(cfgMap)
	if err != nil {
		return nil, err
	}
	emb := module.EmbedderFrom(ctx)
	if emb == nil {
		return nil, fmt.Errorf("embeddings gateway unavailable to the rag worker")
	}
	key := module.AppID(ctx) + "\x00" + hashConfig(cfgMap)

	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	if ent, ok := m.engines[key]; ok {
		ent.usedAt = now
		return ent.eng, nil
	}
	be, err := newBackend(cfg)
	if err != nil {
		return nil, err
	}
	e := NewEngine(cfg, be, emb, module.RerankerFrom(ctx))
	// All sources → the generic indexation service (web/file/database/kafka/
	// codebase connectors + triggers), off the loop, bounded. Nothing syncs
	// on the daemon path. Back-compat: triggersFor maps auto_index → triggers.
	sink := ragSink{eng: e}
	owner := module.AppID(ctx)
	cdsn := cursorDSNFor(cfg)
	var specs []indexer.SourceSpec
	for _, src := range cfg.Sources {
		if spec, ok := specFor(src, cfg.AutoIndex); ok {
			spec.Owner = owner
			spec.CursorDSN = cdsn
			m.idx.Register(spec, sink)
			specs = append(specs, spec)
		}
	}
	m.engines[key] = &engEntry{eng: e, specs: specs, usedAt: now}
	m.evictLocked(now)
	return e, nil
}

// evictLocked bounds the engine cache : TTL-evicts idle engines, then evicts
// the least-recently-used until <= maxEngines. Eviction deregisters the
// engine's source syncs and closes its backend — so 100k apps cost only the
// hot working set of connections/indexes, not 100k live engines. Caller
// holds m.mu ; the just-added engine (usedAt=now) is never evicted.
func (m *Module) evictLocked(now time.Time) {
	for k, ent := range m.engines {
		if now.Sub(ent.usedAt) > engineTTL {
			m.dropLocked(k, ent)
		}
	}
	for len(m.engines) > m.maxEngines {
		var oldestKey string
		var oldest time.Time
		for k, ent := range m.engines {
			if oldestKey == "" || ent.usedAt.Before(oldest) {
				oldestKey, oldest = k, ent.usedAt
			}
		}
		if oldestKey == "" {
			break
		}
		m.dropLocked(oldestKey, m.engines[oldestKey])
	}
}

func (m *Module) dropLocked(key string, ent *engEntry) {
	for _, s := range ent.specs {
		m.idx.Deregister(s)
	}
	_ = ent.eng.Close()
	delete(m.engines, key)
}

func (m *Module) createKB(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.Name)
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	vecs, dim, err := eng.embed.EmbedModel(ctx, eng.model, pkgmoduleRoleDocument, []string{"dimension probe"})
	if err != nil || len(vecs) == 0 || dim == 0 {
		return fail(fmt.Sprintf("probe embedding failed: %v", err)), nil
	}
	if err := eng.backend.EnsureKB(ctx, kb, dim); err != nil {
		return fail(err.Error()), nil
	}
	return ok(map[string]any{"name": kb, "dimension": dim, "model": eng.model, "created": true}), nil
}

func (m *Module) listKBs(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	names, err := eng.backend.ListKBs(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	sort.Strings(names)
	kbs := make([]map[string]any, 0, len(names))
	for _, n := range names {
		info, _ := eng.backend.KBInfo(ctx, n)
		kbs = append(kbs, map[string]any{"name": n, "documents": info.Count, "dimension": info.Dim})
	}
	return ok(map[string]any{"knowledge_bases": kbs, "count": len(kbs)}), nil
}

func (m *Module) kbStats(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.Name)
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	count, err := eng.backend.CountKB(ctx, kb)
	if err != nil {
		return fail(err.Error()), nil
	}
	return ok(map[string]any{"name": kb, "documents": count}), nil
}

func (m *Module) deleteKB(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.Name)
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	if err := eng.backend.DeleteKB(ctx, kb); err != nil {
		return fail(err.Error()), nil
	}
	eng.invalidate(kb)
	return ok(map[string]any{"name": kb, "deleted": true}), nil
}

func (m *Module) ingest(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		KnowledgeBase string         `json:"knowledge_base"`
		Name          string         `json:"name"`
		Text          string         `json:"text"`
		Source        string         `json:"source"`
		Metadata      map[string]any `json:"metadata"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.KnowledgeBase, p.Name)
	if strings.TrimSpace(p.Text) == "" {
		return fail("text is required"), nil
	}
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	n, err := eng.IngestWithMeta(ctx, kb, p.Text, p.Source, p.Metadata)
	if err != nil {
		return fail(err.Error()), nil
	}
	return ok(map[string]any{"knowledge_base": kb, "chunks": n}), nil
}

func (m *Module) ingestFile(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		KnowledgeBase string `json:"knowledge_base"`
		Name          string `json:"name"`
		Path          string `json:"path"`
		Source        string `json:"source"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.KnowledgeBase, p.Name)
	if strings.TrimSpace(p.Path) == "" {
		return fail("path is required"), nil
	}
	loaded, err := LoadFile(p.Path)
	if err != nil {
		return fail(err.Error()), nil
	}
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	src := p.Source
	if src == "" {
		src = filepath.Base(p.Path)
	}
	n, err := eng.Ingest(ctx, kb, loaded.Text, src)
	if err != nil {
		return fail(err.Error()), nil
	}
	return ok(map[string]any{"knowledge_base": kb, "file": p.Path, "chunks": n}), nil
}

func (m *Module) ingestDirectory(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		KnowledgeBase string   `json:"knowledge_base"`
		Name          string   `json:"name"`
		Path          string   `json:"path"`
		Recursive     *bool    `json:"recursive"`
		Extensions    []string `json:"extensions"`
		MaxFiles      int      `json:"max_files"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.KnowledgeBase, p.Name)
	if strings.TrimSpace(p.Path) == "" {
		return fail("path is required"), nil
	}
	recursive := p.Recursive == nil || *p.Recursive
	maxFiles := p.MaxFiles
	if maxFiles <= 0 {
		maxFiles = 1000
	}
	allow := map[string]bool{}
	for _, e := range p.Extensions {
		allow[strings.ToLower(e)] = true
	}
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}

	root := filepath.Clean(p.Path)
	files, chunks := 0, 0
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if !recursive && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if files >= maxFiles {
			return fs.SkipAll
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !SupportedExt(ext) || (len(allow) > 0 && !allow[ext]) {
			return nil
		}
		loaded, lerr := LoadFile(path)
		if lerr != nil {
			return nil // skip files that fail extraction
		}
		rel, _ := filepath.Rel(root, path)
		n, ierr := eng.Ingest(ctx, kb, loaded.Text, filepath.ToSlash(rel))
		if ierr != nil {
			return nil
		}
		files++
		chunks += n
		return nil
	})
	if walkErr != nil {
		return fail(walkErr.Error()), nil
	}
	return ok(map[string]any{"knowledge_base": kb, "files": files, "chunks": chunks}), nil
}

func (m *Module) query(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		KnowledgeBase string `json:"knowledge_base"`
		Name          string `json:"name"`
		Query         string `json:"query"`
		TopK          int    `json:"top_k"`
	}
	_ = json.Unmarshal(raw, &p)
	if strings.TrimSpace(p.Query) == "" {
		return fail("query is required"), nil
	}
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}

	requested := strings.TrimSpace(p.KnowledgeBase)
	if requested == "" {
		requested = strings.TrimSpace(p.Name)
	}
	kbs := resolveKBs(eng.cfg, requested)
	topK := p.TopK
	if topK <= 0 {
		topK = eng.cfg.Pipeline.FinalTopK
	}

	var hits []SearchHit
	for _, kb := range kbs {
		h, err := eng.Query(ctx, kb, p.Query, topK)
		if err != nil {
			continue // a KB that doesn't exist / errors is skipped, not fatal
		}
		for i := range h {
			if h[i].Meta == nil {
				h[i].Meta = map[string]any{}
			}
			h[i].Meta["knowledge_base"] = kb
		}
		hits = append(hits, h...)
	}
	if len(kbs) > 1 {
		sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
		if len(hits) > topK {
			hits = hits[:topK]
		}
	}

	results := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		r := map[string]any{"text": h.Text, "source": h.Source, "chunk": h.Chunk, "score": h.Score}
		if h.Meta != nil {
			if kb, ok := h.Meta["knowledge_base"]; ok {
				r["knowledge_base"] = kb
			}
		}
		results = append(results, r)
	}
	data := map[string]any{"knowledge_bases": kbs, "results": results, "count": len(results)}
	if eng.cfg.Citations.Enabled {
		data["citations"] = formatCitations(hits, eng.cfg.Citations.Format)
	}
	return tool.Result{Success: true, Data: data, Display: &tool.DisplayHint{Type: "json", Title: "RAG: " + p.Query}}, nil
}

// reindex invokes the indexation service to (re)sync every configured source
// of the calling app now (Walk connectors : web/file/database). Stream-only
// sources (kafka, cdc) run continuously and are reported as such. This is the
// service's explicit, on-demand invocation surface (admin / control-plane).
func (m *Module) reindex(ctx context.Context, _ json.RawMessage) (tool.Result, error) {
	cfg, err := ParseConfig(module.ModuleConfigFrom(ctx))
	if err != nil {
		return fail(err.Error()), nil
	}
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	sink := ragSink{eng: eng}
	owner := module.AppID(ctx)
	cdsn := cursorDSNFor(cfg)
	results := make([]map[string]any, 0, len(cfg.Sources))
	total := 0
	for _, src := range cfg.Sources {
		spec, ok := specFor(src, cfg.AutoIndex)
		if !ok {
			continue
		}
		spec.Owner = owner
		spec.CursorDSN = cdsn
		entry := map[string]any{"source": spec.Name, "type": spec.Type}
		rep, err := m.idx.Sync(ctx, spec, sink)
		if err != nil {
			entry["status"] = err.Error()
		} else {
			entry["added"], entry["updated"], entry["deleted"] = rep.Added, rep.Updated, rep.Deleted
			total += rep.Added + rep.Updated
		}
		results = append(results, entry)
	}
	return ok(map[string]any{"sources": results, "reindexed": total}), nil
}

func (m *Module) indexStats(_ context.Context, _ json.RawMessage) (tool.Result, error) {
	st := m.idx.Stats()
	b, _ := json.Marshal(st)
	var data map[string]any
	_ = json.Unmarshal(b, &data)
	return tool.Result{Success: true, Data: data, Display: &tool.DisplayHint{Type: "json", Title: "Indexation metrics"}}, nil
}

func (m *Module) migrateEmbeddings(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p struct {
		KnowledgeBase string `json:"knowledge_base"`
		Name          string `json:"name"`
		Target        string `json:"target"`
		Model         string `json:"model"`
	}
	_ = json.Unmarshal(raw, &p)
	kb := kbName(p.KnowledgeBase, p.Name)
	eng, err := m.engineFor(ctx)
	if err != nil {
		return fail(err.Error()), nil
	}
	rep, err := eng.Migrate(ctx, kb, p.Target, p.Model)
	if err != nil {
		return fail(err.Error()), nil
	}
	return ok(map[string]any{
		"source": rep.Source, "target": rep.Target, "model": rep.Model,
		"migrated": rep.Migrated, "dimension": rep.Dim,
	}), nil
}


func kbName(candidates ...string) string {
	for _, c := range candidates {
		if s := strings.TrimSpace(c); s != "" {
			return s
		}
	}
	return "default"
}

func hashConfig(cfg map[string]any) string {
	if len(cfg) == 0 {
		return "default"
	}
	b, _ := json.Marshal(cfg)
	h := sha1.Sum(b)
	return hex.EncodeToString(h[:8])
}

func ok(data map[string]any) tool.Result { return tool.Result{Success: true, Data: data} }
func fail(msg string) tool.Result        { return tool.Result{Success: false, Error: msg} }
