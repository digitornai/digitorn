// Package catalog enumerates every identifier the compiler validates
// references against: modules, tools, middleware, channel adapters, providers.
// Module data is sourced from pluggable ManifestSources (a directory of YAML
// manifests or the runtime module registry).
package catalog

import (
	"sort"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/pkg/module"
)

// ManifestSource yields module manifests.
type ManifestSource interface {
	Manifests() ([]module.Manifest, error)
}

type ModuleEntry struct {
	Manifest module.Manifest
	Tools    map[string]tool.Spec
}

type Catalog struct {
	modules        map[string]*ModuleEntry
	middleware     map[string]struct{}
	channels       map[string]struct{}
	providers      map[string]struct{}
	hookConditions map[string]HookConditionSpec
	hookActions    map[string]HookActionSpec
	widgetFilters  map[string]struct{}
}

func New(sources ...ManifestSource) (*Catalog, error) {
	mods := map[string]*ModuleEntry{}
	merge := func(ms []module.Manifest) {
		for _, m := range ms {
			entry, ok := mods[m.ID]
			if !ok {
				entry = &ModuleEntry{Manifest: m, Tools: map[string]tool.Spec{}}
				mods[m.ID] = entry
			}
			for _, t := range m.Tools {
				entry.Tools[t.Name] = t
			}
		}
	}
	// Seed the runtime-internal system modules (memory, agent_spawn) so apps
	// can DECLARE them per the documented contract even though they have no
	// bus manifest. A real ManifestSource may override them below.
	merge(systemModuleManifests())
	for _, src := range sources {
		ms, err := src.Manifests()
		if err != nil {
			return nil, err
		}
		merge(ms)
	}
	return &Catalog{
		modules:        mods,
		middleware:     setOf(defaultMiddleware()),
		channels:       setOf(defaultChannels()),
		providers:      setOf(defaultProviders()),
		hookConditions: hookSpecsIndex(HookConditions, func(s HookConditionSpec) string { return s.Name }),
		hookActions:    hookSpecsIndex(HookActions, func(s HookActionSpec) string { return s.Name }),
		widgetFilters:  setOf(WidgetFilters),
	}, nil
}

func Empty() *Catalog {
	c, _ := New()
	return c
}

func (c *Catalog) HasModule(id string) bool { _, ok := c.modules[id]; return ok }

func (c *Catalog) HasTool(moduleID, toolName string) bool {
	entry, ok := c.modules[moduleID]
	if !ok {
		return false
	}
	_, ok = entry.Tools[toolName]
	return ok
}

func (c *Catalog) HasMiddleware(name string) bool  { _, ok := c.middleware[name]; return ok }
func (c *Catalog) HasChannelType(name string) bool { _, ok := c.channels[name]; return ok }
func (c *Catalog) HasProvider(name string) bool    { _, ok := c.providers[name]; return ok }

func (c *Catalog) ModuleIDs() []string { return sortedKeys(c.modules) }

func (c *Catalog) ToolsFor(id string) []string {
	entry, ok := c.modules[id]
	if !ok {
		return nil
	}
	names := make([]string, 0, len(entry.Tools))
	for n := range entry.Tools {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ToolSpec returns the full spec (params, risk, permissions...) for a tool.
func (c *Catalog) ToolSpec(moduleID, toolName string) (tool.Spec, bool) {
	entry, ok := c.modules[moduleID]
	if !ok {
		return tool.Spec{}, false
	}
	spec, ok := entry.Tools[toolName]
	return spec, ok
}

// ConfigSchema returns the module's declared config schema, if any.
func (c *Catalog) ConfigSchema(moduleID string) (map[string]any, bool) {
	entry, ok := c.modules[moduleID]
	if !ok {
		return nil, false
	}
	return entry.Manifest.ConfigSchema, len(entry.Manifest.ConfigSchema) > 0
}

func (c *Catalog) CompatibleMiddleware(moduleID string) ([]string, bool) {
	entry, ok := c.modules[moduleID]
	if !ok {
		return nil, false
	}
	if len(entry.Manifest.CompatibleMiddleware) == 0 {
		return nil, false
	}
	return entry.Manifest.CompatibleMiddleware, true
}

func (c *Catalog) MiddlewareNames() []string { return sortedSet(c.middleware) }
func (c *Catalog) ChannelTypes() []string    { return sortedSet(c.channels) }
func (c *Catalog) Providers() []string       { return sortedSet(c.providers) }

func setOf(list []string) map[string]struct{} {
	out := make(map[string]struct{}, len(list))
	for _, s := range list {
		out[s] = struct{}{}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]struct{}) []string { return sortedKeys(m) }
