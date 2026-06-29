// Package compiler turns a Digitorn YAML manifest into a validated AppDefinition.
package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/bundle"
	"github.com/mbathepaul/digitorn/internal/compiler/catalog"
	"github.com/mbathepaul/digitorn/internal/compiler/codegen"
	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/expr"
	"github.com/mbathepaul/digitorn/internal/compiler/parse"
	"github.com/mbathepaul/digitorn/internal/compiler/refs"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/compiler/validate"

	_ "github.com/mbathepaul/digitorn/internal/compiler/suggest"
)

type Compiler struct {
	Strict          bool
	ManifestSources []catalog.ManifestSource

	mu      sync.Mutex // guards the lazy catalog cache (Compile is concurrency-safe)
	catalog *catalog.Catalog
}

// New creates a Compiler. If no manifest sources are configured by the time a
// compilation runs, it auto-discovers a `manifests/` directory next to the CWD
// and the path from the DIGITORN_MANIFESTS env var.
func New() *Compiler { return &Compiler{} }

func (c *Compiler) WithSources(sources ...catalog.ManifestSource) *Compiler {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ManifestSources = append(c.ManifestSources, sources...)
	c.catalog = nil
	return c
}

// InvalidateCatalog drops the cached catalog so the next compile rebuilds it
// from the (now-current) registry. Called after worker-hosted modules register
// their manifests at startup : those registrations otherwise race the first
// compile, leaving worker modules wrongly reported "unknown module" for apps
// installed afterward.
func (c *Compiler) InvalidateCatalog() {
	c.mu.Lock()
	c.catalog = nil
	c.mu.Unlock()
}

// loadCatalog returns the (lazily built, cached) catalog. The mutex makes it
// safe to call Compile concurrently on one Compiler : the first caller builds,
// the rest read the cache. Holding the lock across catalog.New collapses N
// racing first-callers into a single build instead of N redundant ones.
func (c *Compiler) loadCatalog() (*catalog.Catalog, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.catalog != nil {
		return c.catalog, nil
	}
	sources := c.ManifestSources
	if len(sources) == 0 {
		for _, dir := range defaultManifestDirs() {
			sources = append(sources, catalog.DirSource{Dir: dir})
		}
	}
	cat, err := catalog.New(sources...)
	if err != nil {
		return nil, err
	}
	c.catalog = cat
	return cat, nil
}

func defaultManifestDirs() []string {
	out := []string{}
	if v := os.Getenv("DIGITORN_MANIFESTS"); v != "" {
		out = append(out, v)
	}
	if wd, err := os.Getwd(); err == nil {
		out = append(out, filepath.Join(wd, "manifests"))
	}
	if exe, err := os.Executable(); err == nil {
		out = append(out, filepath.Join(filepath.Dir(exe), "manifests"))
	}
	return out
}

type Result struct {
	File        string
	Bundle      *bundle.Bundle
	Definition  *schema.AppDefinition
	Diagnostics *diagnostic.Bag
}

func (r *Result) OK() bool { return r.Diagnostics != nil && !r.Diagnostics.HasErrors() }

// Compile resolves path to a bundle (directory or app.yaml file) and compiles it.
func (c *Compiler) Compile(path string) (*Result, error) {
	b, err := bundle.Load(path)
	if err != nil {
		return nil, fmt.Errorf("compile: %w", err)
	}
	return c.compileBundle(b)
}

func (c *Compiler) CompileFile(path string) (*Result, error) { return c.Compile(path) }

// Build produces a .dgc artifact from a compiled Result. Fails if the Result
// has any errors.
func (c *Compiler) Build(r *Result) (*codegen.Artifact, error) {
	if r == nil || r.Definition == nil {
		return nil, fmt.Errorf("compiler: cannot build artifact from nil result")
	}
	if !r.OK() {
		return nil, fmt.Errorf("compiler: refusing to build artifact, %d error(s) remain", len(r.Diagnostics.Errors()))
	}
	cat, err := c.loadCatalog()
	if err != nil {
		return nil, err
	}
	return codegen.Build(r.Definition, cat)
}

func (c *Compiler) compileBundle(b *bundle.Bundle) (*Result, error) {
	pf, bag, err := parse.ParseFile(b.Entry)
	if err != nil {
		return nil, fmt.Errorf("compile %s: %w", b.Entry, err)
	}

	def := &schema.AppDefinition{}
	if pf.Document == nil || bag.HasErrors() {
		return &Result{File: b.Entry, Bundle: b, Definition: def, Diagnostics: bag}, nil
	}

	meta := preScanAppMeta(pf.Document)
	vars := preScanDevVariables(pf.Document)
	engine := c.newEngine(b, meta, vars)
	// Flow node ids are runtime namespaces : `{{<node>.output.x}}` templates in
	// flow params/messages are filled by the flow runner, not the compiler.
	engine.AddPassthrough(preScanFlowNodeIDs(pf.Document)...)
	expr.ResolveInTree(b.Entry, pf.Document, engine, bag)

	_ = parse.StrictDecode(b.Entry, pf.Document, def, "", bag)
	parse.CheckUnknownFields(b.Entry, pf.Document, def, "", bag)
	captureModesOrder(pf.Document, def)
	foldLegacyAliases(def)
	mergeIncludedAgents(b, def, bag)
	mergeTemplatesFragment(b, def)
	injectAutoCompact(def) // runtime.context.auto_compact → synthetic compaction hook
	normalizeModuleAliases(def) // legacy module ids (workspace → filesystem) before any validation
	cat, err := c.loadCatalog()
	if err != nil {
		return nil, err
	}
	refs.Check(b.Entry, pf.Document, def, cat, bag)
	validate.Check(b.Entry, pf.Document, def, bag)
	validate.CheckParams(b.Entry, def, cat, bag)
	validate.CheckConfig(b.Entry, def, cat, bag)
	validate.CheckConstraints(b.Entry, def, cat, bag)
	validate.CheckMiddleware(b.Entry, def, cat, bag)
	validate.CheckAppMiddleware(b.Entry, def, bag)
	validate.CheckMCPRefs(b.Entry, def, bag)
	validate.CheckMCPConfig(b.Entry, def, bag)
	validate.CheckHooks(b.Entry, def, cat, bag)
	validate.CheckExpressions(b.Entry, def, bag)
	validate.CheckWidgets(b.Entry, def, cat, bag)
	validate.CheckPromptFrontmatter(b.Root, def, bag)
	validate.CheckHallucinations(b.Entry, pf.Document, bag)

	return &Result{File: b.Entry, Bundle: b, Definition: def, Diagnostics: bag}, nil
}

// captureModesOrder records the YAML insertion order of runtime.modes keys.
// Go maps lose order ; the mode default-policy ("first declared") needs it.
func captureModesOrder(doc *yaml.Node, def *schema.AppDefinition) {
	if def == nil || def.Runtime == nil || len(def.Runtime.Modes) == 0 {
		return
	}
	node := parse.LookupNode(doc, "runtime.modes")
	if node == nil || node.Kind != yaml.MappingNode {
		return
	}
	order := make([]string, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		order = append(order, node.Content[i].Value)
	}
	def.Runtime.ModesOrder = order
}

// foldLegacyAliases rewrites top-level `modules:` / `capabilities:` into the
// canonical `tools.modules` / `tools.capabilities` location.
func foldLegacyAliases(def *schema.AppDefinition) {
	if len(def.ModulesTop) == 0 && def.CapabilitiesTop == nil {
		return
	}
	if def.Tools == nil {
		def.Tools = &schema.ToolsBlock{}
	}
	if len(def.ModulesTop) > 0 {
		if def.Tools.Modules == nil {
			def.Tools.Modules = make(map[string]schema.ModuleBlock, len(def.ModulesTop))
		}
		for k, v := range def.ModulesTop {
			if _, exists := def.Tools.Modules[k]; !exists {
				def.Tools.Modules[k] = v
			}
		}
	}
	if def.CapabilitiesTop != nil && def.Tools.Capabilities == nil {
		def.Tools.Capabilities = def.CapabilitiesTop
	}
	def.ModulesTop = nil
	def.CapabilitiesTop = nil

	// Normalise hook event alias: when a user writes `on:` and the editor
	// saves it as bare YAML 1.1, it round-trips back as boolean key `true:`.
	for i, a := range def.Agents {
		for j := range a.Hooks {
			normaliseHookOn(&def.Agents[i].Hooks[j])
		}
	}
	if def.Runtime != nil {
		for i := range def.Runtime.Hooks {
			normaliseHookOn(&def.Runtime.Hooks[i])
		}
	}
}

func normaliseHookOn(h *schema.Hook) {
	if h.On == "" {
		switch {
		case h.Event != "":
			h.On = h.Event
		case h.OnTrue != "":
			h.On = h.OnTrue
		}
	}
}

// mergeTemplatesFragment loads a sibling `templates.yaml` (convention file,
// mirroring the legacy daemon) into def.Templates when not declared inline.
func mergeTemplatesFragment(b *bundle.Bundle, def *schema.AppDefinition) {
	for _, name := range []string{"templates.yaml", "templates.yml"} {
		data, err := os.ReadFile(filepath.Join(b.Root, name))
		if err != nil {
			continue
		}
		var frag struct {
			Templates []schema.TemplateBlock `yaml:"templates"`
		}
		if err := yaml.Unmarshal(data, &frag); err != nil {
			return
		}
		seen := map[string]bool{}
		for _, t := range def.Templates {
			seen[t.ID] = true
		}
		for _, t := range frag.Templates {
			if t.ID == "" || seen[t.ID] {
				continue
			}
			seen[t.ID] = true
			def.Templates = append(def.Templates, t)
		}
		return
	}
}

func mergeIncludedAgents(b *bundle.Bundle, def *schema.AppDefinition, bag *diagnostic.Bag) {
	loadAgentsInto(b, "agents", def, bag)
	loadHooksInto(b, "hooks", def, bag)
	if def.Dev == nil || def.Dev.Include == nil {
		return
	}
	for _, path := range includePaths(def.Dev.Include.Agents) {
		loadAgentsInto(b, path, def, bag)
	}
	for _, path := range includePaths(def.Dev.Include.Hooks) {
		loadHooksInto(b, path, def, bag)
	}
}

func includePaths(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func loadAgentsInto(b *bundle.Bundle, rel string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	abs := filepath.Join(b.Root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	if info.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
				continue
			}
			loadAgentFile(filepath.Join(abs, name), def, bag)
		}
		return
	}
	loadAgentFile(abs, def, bag)
}

func loadAgentFile(path string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var agent schema.Agent
	if err := yaml.Unmarshal(data, &agent); err != nil {
		return
	}
	if agent.ID == "" {
		return
	}
	for _, a := range def.Agents {
		if a.ID == agent.ID {
			return
		}
	}
	def.Agents = append(def.Agents, agent)
}

func loadHooksInto(b *bundle.Bundle, rel string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	abs := filepath.Join(b.Root, rel)
	info, err := os.Stat(abs)
	if err != nil {
		return
	}
	if info.IsDir() {
		entries, err := os.ReadDir(abs)
		if err != nil {
			return
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || (!strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml")) {
				continue
			}
			loadHookFile(filepath.Join(abs, name), def, bag)
		}
		return
	}
	loadHookFile(abs, def, bag)
}

func loadHookFile(path string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var single schema.Hook
	if err := yaml.Unmarshal(data, &single); err == nil && single.On != "" {
		appendHook(def, single)
		return
	}
	var multi []schema.Hook
	if err := yaml.Unmarshal(data, &multi); err == nil {
		for _, h := range multi {
			if h.On != "" {
				appendHook(def, h)
			}
		}
		return
	}
	var wrapped struct {
		Hooks []schema.Hook `yaml:"hooks"`
	}
	if err := yaml.Unmarshal(data, &wrapped); err == nil {
		for _, h := range wrapped.Hooks {
			if h.On != "" {
				appendHook(def, h)
			}
		}
	}
}

func appendHook(def *schema.AppDefinition, h schema.Hook) {
	normaliseHookOn(&h)
	if def.Runtime == nil {
		def.Runtime = &schema.RuntimeBlock{}
	}
	for _, existing := range def.Runtime.Hooks {
		if existing.ID != "" && existing.ID == h.ID {
			return
		}
	}
	def.Runtime.Hooks = append(def.Runtime.Hooks, h)
}

func (c *Compiler) newEngine(b *bundle.Bundle, meta map[string]string, vars map[string]string) *expr.Engine {
	e := expr.NewEngine()
	if c.Strict {
		// CI / release path : missing env vars fail compile-time.
		e.Register("env", expr.StrictEnvResolver())
	} else {
		// Dev / lenient path : missing env vars passthrough so the
		// runtime can resolve them (matches Python compiler).
		e.Register("env", expr.EnvResolver())
	}
	e.Register("sys", expr.SysResolver())
	e.Register("app", expr.LenientMapResolver("app", meta))
	e.Register("var", expr.LenientMapResolver("var", vars))
	e.Register("secret", expr.SecretResolver(nil))
	e.Register("prompt", bundle.PromptResolver(b))
	e.Register("skill", bundle.SkillResolver(b))
	e.Register("behavior", bundle.BehaviorResolver(b))
	e.Register("asset", bundle.AssetResolver(b, meta["id"]))
	e.Register("asset_b64", bundle.AssetBase64Resolver(b))
	e.SetIncludeResolver(bundle.IncludeResolver(b))
	return e
}

func preScanAppMeta(doc *yaml.Node) map[string]string {
	out := map[string]string{}
	if !parse.IsMapping(doc) {
		return out
	}
	_, appNode, ok := parse.FindKey(doc, "app")
	if !ok || !parse.IsMapping(appNode) {
		return out
	}
	pairs := map[string]string{
		"app_id":      "id",
		"name":        "name",
		"version":     "version",
		"author":      "author",
		"description": "description",
		"short_name":  "short_name",
	}
	for yamlKey, varKey := range pairs {
		if _, n, ok := parse.FindKey(appNode, yamlKey); ok && parse.IsScalar(n) {
			out[varKey] = n.Value
		}
	}
	return out
}

// preScanFlowNodeIDs reads flow.nodes[].id before decode so the template engine
// can treat them as runtime passthrough namespaces.
func preScanFlowNodeIDs(doc *yaml.Node) []string {
	if !parse.IsMapping(doc) {
		return nil
	}
	flowNode := func() *yaml.Node {
		if _, n, ok := parse.FindKey(doc, "flow"); ok {
			return n
		}
		// Legacy: runtime.flow
		if _, rt, ok := parse.FindKey(doc, "runtime"); ok && parse.IsMapping(rt) {
			if _, n, ok := parse.FindKey(rt, "flow"); ok {
				return n
			}
		}
		return nil
	}()
	if flowNode == nil || !parse.IsMapping(flowNode) {
		return nil
	}
	_, nodes, ok := parse.FindKey(flowNode, "nodes")
	if !ok || nodes.Kind != yaml.SequenceNode {
		return nil
	}
	var ids []string
	for _, n := range nodes.Content {
		if !parse.IsMapping(n) {
			continue
		}
		if _, idNode, ok := parse.FindKey(n, "id"); ok && parse.IsScalar(idNode) {
			ids = append(ids, idNode.Value)
		}
	}
	return ids
}

func preScanDevVariables(doc *yaml.Node) map[string]string {
	out := map[string]string{}
	if !parse.IsMapping(doc) {
		return out
	}
	_, devNode, ok := parse.FindKey(doc, "dev")
	if !ok || !parse.IsMapping(devNode) {
		return out
	}
	_, varsNode, ok := parse.FindKey(devNode, "variables")
	if !ok || !parse.IsMapping(varsNode) {
		return out
	}
	for i := 0; i < len(varsNode.Content)-1; i += 2 {
		k, v := varsNode.Content[i], varsNode.Content[i+1]
		if parse.IsScalar(k) && parse.IsScalar(v) {
			out[k.Value] = v.Value
		}
	}
	return out
}
