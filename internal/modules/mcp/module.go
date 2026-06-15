// Package mcp connects external Model Context Protocol servers (stdio or http)
// and exposes their tools to the agent as first-class native digitorn tools.
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	domainmodule "github.com/mbathepaul/digitorn/internal/domain/module"
	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/pkg/module"
)

const (
	moduleID          = "mcp"
	moduleVersion     = "1.0.0"
	defaultMaxRetries = 4
	healthInterval    = 60 * time.Second
	defaultTimeout    = 30 * time.Second

	// userKeySep joins a server id with a user id for per-user stdio pools.
	// NUL can't appear in a server id, so the key is unambiguous.
	userKeySep = "\x00"
	// maxUserPools caps concurrent per-user stdio subprocesses (LRU-evicted).
	maxUserPools = 20
)

// virtualRe splits "mcp_<server>__<tool>"; the server group stops before "__"
// so mcp_google_calendar__list_events resolves correctly.
var virtualRe = regexp.MustCompile(`^mcp_([^_]+(?:_[^_]+)*)__(.+)$`)

type Module struct {
	module.Base

	mu         sync.RWMutex
	pool       *pool
	healthStop chan struct{}
}

func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          moduleID,
		Version:     moduleVersion,
		Description: "Connect external Model Context Protocol servers; their tools become native agent tools.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
	}
	m.pool = newPool(defaultMaxRetries)
	return m
}

func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	servers, _ := schema.NormalizeServers(cfg["servers"])
	autoInstall := autoInstallEnabled(cfg)
	for id, sc := range servers {
		spec, ok := m.resolveServer(ctx, id, sc, autoInstall)
		if !ok {
			continue
		}
		_, _ = m.pool.connect(ctx, id, spec)
	}
	return nil
}

func autoInstallEnabled(cfg map[string]any) bool {
	v, _ := cfg["auto_install"].(bool)
	return v
}

func (m *Module) Start(ctx context.Context) error {
	m.startHealth()
	return m.Base.Start(ctx)
}

func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.healthStop != nil {
		close(m.healthStop)
		m.healthStop = nil
	}
	pool := m.pool
	m.mu.Unlock()
	if pool != nil {
		pool.shutdown(ctx)
	}
	return m.Base.Stop(ctx)
}

// Invoke routes a virtual-tool call (mcp_<server>__<tool>) to the owning server.
func (m *Module) Invoke(ctx context.Context, toolName string, params []byte) (tool.Result, error) {
	server, action, ok := parseVirtual(toolName)
	if !ok {
		return tool.Result{Success: false, Error: "mcp: unroutable tool " + toolName},
			fmt.Errorf("mcp: unroutable tool %q", toolName)
	}
	m.ensureConnected(ctx, server)
	// Route to the physical connection: bare server id for http/non-auth, or a
	// per-user key for an authenticated stdio server (its token rides its env).
	key := m.routeKey(ctx, server)

	var args map[string]any
	if len(params) > 0 {
		_ = json.Unmarshal(params, &args)
	}

	switch action {
	case "list_prompts":
		return wrapJSON(server, map[string]any{"server_id": server, "prompts": m.pool.promptsOf(key)}), nil
	case "get_prompt":
		name, _ := args["prompt_name"].(string)
		res, err := m.pool.getPrompt(ctx, key, name, toStringMap(args["arguments"]))
		if err != nil {
			return failResult(err), nil
		}
		return wrapJSON(server, res), nil
	case "list_resources":
		return wrapJSON(server, map[string]any{"server_id": server, "resources": m.pool.resourcesOf(key)}), nil
	case "read_resource":
		uri, _ := args["uri"].(string)
		res, err := m.pool.readResource(ctx, key, uri)
		if err != nil {
			return failResult(err), nil
		}
		return wrapJSON(server, res), nil
	}

	res, err := m.pool.callTool(ctx, key, action, args)
	if err != nil {
		// The connection was dropped, stale, or never came up (a transport error —
		// tool-level errors ride res.IsError, not err). Reconnect the target once
		// and retry, so a dead pooled connection self-heals instead of failing every
		// call until a restart. A permanent fault (bad auth, missing server) fails
		// the reconnect fast (non-retryable short-circuit), so this never loops.
		if _, rerr := m.pool.reconnect(ctx, key); rerr == nil {
			res, err = m.pool.callTool(ctx, key, action, args)
		}
		if err != nil {
			return failResult(err), nil
		}
	}
	return wrapResult(server, action, res), nil
}

// isStdioOAuth reports whether a server runs over stdio with an oauth2 block —
// the only servers that need a per-user subprocess (their token is an env var).
func isStdioOAuth(sc schema.MCPServerConfig) bool {
	return normTransport(string(sc.Transport)) == "stdio" && sc.Auth != nil && sc.Auth.Type == "oauth2"
}

// physicalKey is the pool key for a server: per-user for authenticated stdio,
// shared (bare id) for http and non-auth stdio.
func physicalKey(serverID string, sc schema.MCPServerConfig, userID string) string {
	if userID != "" && isStdioOAuth(sc) {
		return serverID + userKeySep + userID
	}
	return serverID
}

// routeKey resolves the physical pool key for a logical server on this call.
func (m *Module) routeKey(ctx context.Context, server string) string {
	cfg := module.ModuleConfigFrom(ctx)
	if len(cfg) == 0 {
		return server
	}
	servers, _ := schema.NormalizeServers(cfg["servers"])
	sc, ok := servers[server]
	if !ok {
		return server
	}
	return physicalKey(server, sc, callerUserID(ctx))
}

func callerUserID(ctx context.Context) string {
	if id, ok := tool.IdentityFromContext(ctx); ok {
		return id.UserID
	}
	return ""
}

func toStringMap(v any) map[string]string {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, val := range m {
		if s, ok := val.(string); ok {
			out[k] = s
		} else {
			out[k] = fmt.Sprintf("%v", val)
		}
	}
	return out
}

func parseVirtual(name string) (server, action string, ok bool) {
	g := virtualRe.FindStringSubmatch(name)
	if g == nil {
		return "", "", false
	}
	return g[1], g[2], true
}

func toConnectSpec(sc schema.MCPServerConfig) (connectSpec, bool) {
	if sc.Command == "" && sc.URL == "" {
		return connectSpec{}, false
	}
	timeout := defaultTimeout
	if sc.Timeout > 0 {
		timeout = time.Duration(sc.Timeout * float64(time.Second))
	}
	return connectSpec{
		Transport: string(sc.Transport),
		Command:   sc.Command,
		Args:      sc.Args,
		Env:       sc.Env,
		URL:       sc.URL,
		Headers:   sc.Headers,
		Timeout:   timeout,
	}, true
}

// ensureConnected lazily connects the calling app's declared servers from the
// per-call config. http / non-auth servers share one connection (left alone once
// up). An authenticated stdio server gets a per-user subprocess with its token
// baked into the env; if the user's token changed, it is reconnected.
// ensureConnected lazily connects the calling app's declared servers. With
// onlyServer set it connects JUST that server (the invoke path — so a tool call
// never connects sibling servers with the wrong per-call token); with "" it
// connects them all (the listing path).
func (m *Module) ensureConnected(ctx context.Context, onlyServer string) {
	cfg := module.ModuleConfigFrom(ctx)
	if len(cfg) == 0 {
		return
	}
	servers, _ := schema.NormalizeServers(cfg["servers"])
	autoInstall := autoInstallEnabled(cfg)
	userID := callerUserID(ctx)
	authCtx, _ := module.AuthContextFrom(ctx)
	listingAuth := module.ListingAuthFrom(ctx)
	for id, sc := range servers {
		if onlyServer != "" && id != onlyServer {
			continue
		}
		spec, ok := m.resolveServer(ctx, id, sc, autoInstall)
		if !ok {
			continue
		}
		key := physicalKey(id, sc, userID)
		stdioAuth := userID != "" && isStdioOAuth(sc)
		if stdioAuth {
			ce, _ := catalogLookup(id)
			spec = m.applyServerAuth(spec, id, sc, ce, authCtx)
		}
		// Per-server listing credential: bake THIS server's own token into its
		// connection headers so an app wiring several OAuth servers materializes
		// every one (each with its own credential), not just the first.
		listingAuthed := false
		if ac, has := listingAuth[id]; has && ac.Token != "" {
			spec = withListingAuthHeader(spec, ac)
			listingAuthed = true
		}
		authed := stdioAuth || listingAuthed
		if existing, up := m.pool.get(key); up {
			if !authed {
				continue // http / non-auth: leave as-is
			}
			if existing.spec.AuthFP == spec.AuthFP {
				continue // unchanged credential
			}
			// credential changed or only now available → reconnect (also refreshes
			// a connection cached in a failed/empty state before authorization)
		} else if stdioAuth {
			m.pool.evictOldestUserConn(userKeySep, maxUserPools)
		}
		_, _ = m.pool.connect(ctx, key, spec)
	}
}

// withListingAuthHeader bakes a per-server credential into the connection's
// Authorization header (canonical "Bearer" scheme) and fingerprints it, so each
// server in a multi-server listing connects with its own token and a connection
// cached before the token appeared is reconnected. Copies Headers (never mutates
// the caller's map).
func withListingAuthHeader(spec connectSpec, ac module.AuthContext) connectSpec {
	scheme := ac.TokenType
	if scheme == "" || strings.EqualFold(scheme, "bearer") {
		scheme = "Bearer"
	}
	h := make(map[string]string, len(spec.Headers)+1)
	maps.Copy(h, spec.Headers)
	h["Authorization"] = scheme + " " + ac.Token
	spec.Headers = h
	spec.AuthFP = ac.Token
	return spec
}

func (m *Module) startHealth() {
	m.mu.Lock()
	if m.healthStop != nil {
		m.mu.Unlock()
		return
	}
	stop := make(chan struct{})
	m.healthStop = stop
	pool := m.pool
	m.mu.Unlock()

	go func() {
		t := time.NewTicker(healthInterval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				hctx, cancel := context.WithTimeout(context.Background(), healthInterval)
				for _, id := range pool.healthCheck(hctx) {
					pool.reconnect(hctx, id)
				}
				cancel()
			}
		}
	}()
}
