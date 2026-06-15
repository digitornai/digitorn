package mcp

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// This file is the EXPORTED management surface the daemon's /api/mcp routes call:
// catalog browsing, registry search/browse, and pre-install requirements. It is
// read-only and stateless (no managed-server store) — it reuses the same catalog,
// registry and auth-detection logic the runtime resolver uses.

// CatalogInfo summarizes one static-catalog server for the management API.
type CatalogInfo struct {
	ServerID       string            `json:"server_id"`
	DisplayName    string            `json:"display_name"`
	Description    string            `json:"description"`
	Transport      string            `json:"transport"`
	Runtime        string            `json:"runtime,omitempty"`
	Package        string            `json:"package,omitempty"`
	Command        string            `json:"command,omitempty"`
	Args           []string          `json:"args,omitempty"`
	HasOAuth       bool              `json:"has_oauth"`
	OAuthProvider  string            `json:"oauth_provider,omitempty"`
	OAuthScopes    []string          `json:"oauth_scopes,omitempty"`
	EnvMapping     map[string]string `json:"env_mapping,omitempty"`
	RequiredFields []string          `json:"required_fields"`
	// Hub-sourced trust signals (false/empty for the static fallback catalog).
	Source   string `json:"source,omitempty"` // hub | static
	Verified bool   `json:"verified"`
	Hosted   bool   `json:"hosted"` // Digitorn-hosted → zero-setup install
	Icon     string `json:"icon,omitempty"`
	Category string `json:"category,omitempty"`
}

func catalogInfoOf(id string, e catalogEntry) CatalogInfo {
	required := make([]string, 0, len(e.EnvMapping))
	for k := range e.EnvMapping {
		required = append(required, k)
	}
	sort.Strings(required)
	return CatalogInfo{
		ServerID:       id,
		DisplayName:    firstNonEmpty(e.DisplayName, id),
		Description:    e.Description,
		Transport:      normTransport(e.Transport),
		Runtime:        e.Runtime,
		Package:        e.Package,
		Command:        e.Command,
		Args:           e.Args,
		HasOAuth:       e.OAuthProvider != "",
		OAuthProvider:  e.OAuthProvider,
		OAuthScopes:    e.OAuthScopes,
		EnvMapping:     e.EnvMapping,
		RequiredFields: required,
	}
}

// CatalogList returns the full static catalog, sorted by id.
func CatalogList() []CatalogInfo {
	out := make([]CatalogInfo, 0, len(catalog))
	for id, e := range catalog {
		out = append(out, catalogInfoOf(id, e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })
	return out
}

// CatalogGet returns one catalog entry (false when not a static-catalog server).
func CatalogGet(id string) (CatalogInfo, bool) {
	e, ok := catalog[id]
	if !ok {
		return CatalogInfo{}, false
	}
	return catalogInfoOf(id, e), true
}

// SearchResult is one hit from Search (catalog or registry).
type SearchResult struct {
	ServerID    string `json:"server_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Source      string `json:"source"` // catalog | registry
	Runtime     string `json:"runtime,omitempty"`
	Package     string `json:"package,omitempty"`
	Transport   string `json:"transport,omitempty"`
}

// Search matches the query against the static catalog (substring on id/name/
// description) and appends the best registry hit when not already a catalog
// server. Empty / "*" lists the whole catalog.
func Search(ctx context.Context, query string) []SearchResult {
	q := strings.ToLower(strings.TrimSpace(query))
	out := []SearchResult{}
	seen := map[string]bool{}
	for id, e := range catalog {
		if q == "" || q == "*" || q == "all" ||
			strings.Contains(strings.ToLower(id), q) ||
			strings.Contains(strings.ToLower(e.DisplayName), q) ||
			strings.Contains(strings.ToLower(e.Description), q) {
			out = append(out, SearchResult{
				ServerID: id, Name: firstNonEmpty(e.DisplayName, id), Description: e.Description,
				Source: "catalog", Runtime: e.Runtime, Package: e.Package, Transport: normTransport(e.Transport),
			})
			seen[id] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ServerID < out[j].ServerID })

	if q != "" && q != "*" && q != "all" {
		if srv, ok := searchRegistry(ctx, query); ok && srv != nil {
			rid := registryShortID(srv.Name)
			if rid != "" && !seen[rid] {
				out = append(out, registrySearchResult(rid, srv))
			}
		}
	}
	return out
}

// RegistryBrowse returns a page of the official MCP registry, mapped to search
// results. cursor/limit page through it.
func RegistryBrowse(ctx context.Context, query, cursor string, limit int) ([]SearchResult, string) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	servers, next := listRegistry(ctx, query, cursor, limit)
	out := make([]SearchResult, 0, len(servers))
	for i := range servers {
		s := &servers[i]
		out = append(out, registrySearchResult(registryShortID(s.Name), s))
	}
	return out, next
}

// Requirement is one credential/config field a server needs before it works.
type Requirement struct {
	Key         string `json:"key"`      // the shorthand the user sets (e.g. "token")
	EnvVar      string `json:"env_var"`  // the env var the server actually reads
	Description string `json:"description,omitempty"`
	Required    bool   `json:"required"`
	IsArg       bool   `json:"is_arg"` // positional arg rather than env var
}

// ServerRequirements is the pre-install answer to "what does this server need?".
type ServerRequirements struct {
	ServerID      string        `json:"server_id"`
	DisplayName   string        `json:"display_name"`
	Description   string        `json:"description,omitempty"`
	Source        string        `json:"source"` // catalog | registry
	Transport     string        `json:"transport"`
	Runtime       string        `json:"runtime,omitempty"`
	Package       string        `json:"package,omitempty"`
	Credentials   []Requirement `json:"credentials"`
	OAuth         bool          `json:"oauth"`
	OAuthProvider string        `json:"oauth_provider,omitempty"`
	YAMLExample   string        `json:"yaml_example"`
}

// Requirements derives the credentials/env/OAuth a server needs, before any
// install — from the static catalog if known, else from the registry (with env
// names mapped to shorthands and OAuth auto-detected). Returns ok=false when the
// id is in neither.
func Requirements(ctx context.Context, id string) (ServerRequirements, bool) {
	if e, ok := catalog[id]; ok {
		return requirementsFromCatalog(id, e), true
	}
	if srv, ok := searchRegistry(ctx, id); ok && srv != nil {
		return requirementsFromRegistry(id, srv), true
	}
	return ServerRequirements{}, false
}

func requirementsFromCatalog(id string, e catalogEntry) ServerRequirements {
	creds := make([]Requirement, 0, len(e.EnvMapping))
	keys := make([]string, 0, len(e.EnvMapping))
	for k := range e.EnvMapping {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		env := e.EnvMapping[k]
		creds = append(creds, Requirement{Key: k, EnvVar: env, Required: true, IsArg: env == argAppend})
	}
	return ServerRequirements{
		ServerID: id, DisplayName: firstNonEmpty(e.DisplayName, id), Description: e.Description,
		Source: "catalog", Transport: normTransport(e.Transport), Runtime: e.Runtime, Package: e.Package,
		Credentials: creds, OAuth: e.OAuthProvider != "", OAuthProvider: e.OAuthProvider,
		YAMLExample: yamlExample(id, creds, e.OAuthProvider != ""),
	}
}

func requirementsFromRegistry(id string, srv *registryServer) ServerRequirements {
	var names []string
	var pkg, runtime string
	for i := range srv.Packages {
		p := &srv.Packages[i]
		for _, ev := range p.envVars() {
			names = append(names, ev.Name)
		}
		if pkg == "" {
			switch strings.ToLower(p.regType()) {
			case "npm":
				pkg, runtime = p.Identifier, "npm"
			case "pip", "pypi":
				pkg, runtime = p.Identifier, "pip"
			}
		}
	}
	creds := make([]Requirement, 0, len(names))
	for _, n := range names {
		short := n
		if cands := envVarToShorthands(n); len(cands) > 0 {
			short = cands[0]
		}
		creds = append(creds, Requirement{Key: short, EnvVar: n, Required: true})
	}
	det := detectOAuthFromEnvVars(names)
	transport := "stdio"
	if pkg == "" && len(srv.Remotes) > 0 {
		transport = "streamable_http"
	}
	provider := ""
	if det != nil {
		provider = det.Provider
	}
	return ServerRequirements{
		ServerID: id, DisplayName: firstNonEmpty(srv.Name, id), Description: srv.Description,
		Source: "registry", Transport: transport, Runtime: runtime, Package: pkg,
		Credentials: creds, OAuth: det != nil, OAuthProvider: provider,
		YAMLExample: yamlExample(id, creds, det != nil),
	}
}

// yamlExample renders copy-paste shorthand YAML for an app's mcp config block.
func yamlExample(id string, creds []Requirement, oauth bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", id)
	if oauth {
		b.WriteString("  auth: { type: oauth2 }\n")
	}
	for _, c := range creds {
		if c.IsArg {
			fmt.Fprintf(&b, "  %s: \"<value>\"\n", c.Key)
			continue
		}
		fmt.Fprintf(&b, "  %s: \"{{env.%s}}\"\n", c.Key, c.EnvVar)
	}
	if len(creds) == 0 && !oauth {
		b.WriteString("  {}\n")
	}
	return b.String()
}

func registrySearchResult(id string, srv *registryServer) SearchResult {
	runtime, pkg, transport := "", "", "streamable_http"
	for i := range srv.Packages {
		switch strings.ToLower(srv.Packages[i].regType()) {
		case "npm":
			runtime, pkg, transport = "npm", srv.Packages[i].Identifier, "stdio"
		case "pip", "pypi":
			runtime, pkg, transport = "pip", srv.Packages[i].Identifier, "stdio"
		}
	}
	return SearchResult{
		ServerID: id, Name: firstNonEmpty(srv.Name, id), Description: srv.Description,
		Source: "registry", Runtime: runtime, Package: pkg, Transport: transport,
	}
}

// Resolution is a catalog/registry server resolved into a storable managed-server
// spec, with credential-derived env values separated out (to be sealed).
type Resolution struct {
	ServerID    string
	DisplayName string
	Source      string
	Transport   string
	Command     string
	Args        []string
	URL         string
	Env         map[string]string // non-secret config
	Secrets     map[string]string // credential env-var → value (sealed at rest)
	AuthType    string            // "" | oauth2 | token
	Package     string
}

// ResolveInstall resolves a catalog/registry server id + the user's shorthand
// credentials into a storable managed-server spec — the SAME resolution the
// runtime uses — separating credential-derived env values (sealed) from
// non-secret config. ok=false when the id is in neither catalog nor registry.
func ResolveInstall(ctx context.Context, id string, credentials map[string]string) (Resolution, bool) {
	req, ok := Requirements(ctx, id)
	if !ok {
		return Resolution{}, false
	}
	extra := make(map[string]any, len(credentials))
	for k, v := range credentials {
		extra[k] = v
	}
	spec, ok := resolveServerSpec(ctx, id, schema.MCPServerConfig{Extra: extra}, false)
	if !ok {
		return Resolution{}, false
	}
	secretVars := map[string]bool{}
	for _, c := range req.Credentials {
		if !c.IsArg && c.EnvVar != "" {
			secretVars[c.EnvVar] = true
		}
	}
	env := map[string]string{}
	secrets := map[string]string{}
	for k, v := range spec.Env {
		if secretVars[k] {
			secrets[k] = v
		} else {
			env[k] = v
		}
	}
	authType := ""
	switch {
	case req.OAuth:
		authType = "oauth2"
	case len(secrets) > 0:
		authType = "token"
	}
	return Resolution{
		ServerID: id, DisplayName: req.DisplayName, Source: req.Source,
		Transport: normTransport(spec.Transport), Command: spec.Command, Args: spec.Args, URL: spec.URL,
		Env: env, Secrets: secrets, AuthType: authType, Package: req.Package,
	}, true
}

// ProbeInput is a fully-resolved server connection to test-dial (no catalog /
// registry resolution — a managed server is already resolved).
type ProbeInput struct {
	Transport string
	Command   string
	Args      []string
	Env       map[string]string
	URL       string
	Headers   map[string]string
	Timeout   time.Duration
}

// ProbeResult is the outcome of a connectivity test: a clean dial + tools/list.
type ProbeResult struct {
	OK        bool     `json:"ok"`
	ToolCount int      `json:"tool_count"`
	ToolNames []string `json:"tool_names"`
	Error     string   `json:"error,omitempty"`
}

// ProbeConfig dials a server once and lists its tools, then disconnects — a
// connectivity check the management API runs before an app relies on a server.
// It never touches the live pool. A failed dial / list returns OK=false with the
// error message (no panic, no leaked connection).
func ProbeConfig(ctx context.Context, in ProbeInput) ProbeResult {
	spec := connectSpec{
		Transport: normTransport(in.Transport),
		Command:   in.Command,
		Args:      in.Args,
		Env:       in.Env,
		URL:       in.URL,
		Headers:   in.Headers,
		Timeout:   in.Timeout,
	}
	if spec.Timeout == 0 {
		spec.Timeout = defaultTimeout
	}
	c, err := dial(ctx, spec)
	if err != nil {
		return ProbeResult{OK: false, Error: err.Error()}
	}
	defer c.close()
	tctx, cancel := context.WithTimeout(ctx, capTimeout)
	defer cancel()
	tools, err := c.listTools(tctx)
	if err != nil {
		return ProbeResult{OK: false, Error: err.Error()}
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		if t != nil {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	return ProbeResult{OK: true, ToolCount: len(names), ToolNames: names}
}

// registryShortID is the last path segment of a registry server name
// (io.github.x/cool → cool), the id a user references.
func registryShortID(name string) string {
	if i := strings.LastIndexByte(name, '/'); i >= 0 {
		return name[i+1:]
	}
	return name
}
