package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

var registryURL = "https://registry.modelcontextprotocol.io/v0/servers"

const registryTTL = 5 * time.Minute

type registryEnvVar struct {
	Name       string `json:"name"`
	IsRequired bool   `json:"is_required"`
}

type registryHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type registryPackage struct {
	RegistryType  string           `json:"registry_type"`
	RegistryType2 string           `json:"registryType"`
	Identifier    string           `json:"identifier"`
	EnvVars       []registryEnvVar `json:"environment_variables"`
	EnvVars2      []registryEnvVar `json:"environmentVariables"`
}

func (p registryPackage) regType() string  { return firstNonEmpty(p.RegistryType, p.RegistryType2) }
func (p registryPackage) envVars() []registryEnvVar {
	if len(p.EnvVars) > 0 {
		return p.EnvVars
	}
	return p.EnvVars2
}

type registryRemote struct {
	Type    string           `json:"transport_type"`
	Type2   string           `json:"type"`
	URL     string           `json:"url"`
	Headers []registryHeader `json:"headers"`
}

func (r registryRemote) transport() string { return firstNonEmpty(r.Type, r.Type2) }

type registryServer struct {
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Packages    []registryPackage `json:"packages"`
	Remotes     []registryRemote  `json:"remotes"`
}

type registrySearchResponse struct {
	Servers  []json.RawMessage `json:"servers"`
	Metadata struct {
		NextCursor string `json:"next_cursor"`
	} `json:"metadata"`
}

func unwrapServer(raw json.RawMessage) registryServer {
	var wrapped struct {
		Server registryServer `json:"server"`
	}
	if json.Unmarshal(raw, &wrapped) == nil && (wrapped.Server.Name != "" || len(wrapped.Server.Packages) > 0 || len(wrapped.Server.Remotes) > 0) {
		return wrapped.Server
	}
	var flat registryServer
	_ = json.Unmarshal(raw, &flat)
	return flat
}

var (
	registryMu    sync.Mutex
	registryCache = map[string]registryCacheEntry{}
)

type registryCacheEntry struct {
	srv *registryServer
	at  time.Time
}

func searchRegistry(ctx context.Context, serverID string) (*registryServer, bool) {
	registryMu.Lock()
	if e, ok := registryCache[serverID]; ok && nowSince(e.at) < registryTTL {
		registryMu.Unlock()
		return e.srv, e.srv != nil
	}
	registryMu.Unlock()

	srv := fetchRegistry(ctx, serverID)

	registryMu.Lock()
	registryCache[serverID] = registryCacheEntry{srv: srv, at: time.Now()}
	registryMu.Unlock()
	return srv, srv != nil
}

func nowSince(t time.Time) time.Duration { return time.Since(t) }

func listRegistry(ctx context.Context, query, cursor string, limit int) ([]registryServer, string) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	if s := strings.TrimSpace(query); s != "" && s != "*" {
		q.Set("search", s)
	}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil, ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, ""
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, ""
	}
	var sr registrySearchResponse
	if json.Unmarshal(body, &sr) != nil {
		return nil, ""
	}
	out := make([]registryServer, 0, len(sr.Servers))
	seen := map[string]bool{}
	for _, raw := range sr.Servers {
		srv := unwrapServer(raw)
		if srv.Name == "" && len(srv.Packages) == 0 && len(srv.Remotes) == 0 {
			continue
		}
		if srv.Name != "" {
			if seen[srv.Name] {
				continue
			}
			seen[srv.Name] = true
		}
		out = append(out, srv)
	}
	return out, sr.Metadata.NextCursor
}

func fetchRegistry(ctx context.Context, serverID string) *registryServer {
	q := url.Values{"search": {serverID}, "limit": {"5"}}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, registryURL+"?"+q.Encode(), nil)
	if err != nil {
		return nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	return matchRegistry(body, serverID)
}

func matchRegistry(body []byte, serverID string) *registryServer {
	var sr registrySearchResponse
	if json.Unmarshal(body, &sr) != nil || len(sr.Servers) == 0 {
		return nil
	}
	want := strings.ReplaceAll(serverID, "_", "-")
	var first *registryServer
	for _, raw := range sr.Servers {
		srv := unwrapServer(raw)
		if srv.Name == "" && len(srv.Packages) == 0 && len(srv.Remotes) == 0 {
			continue
		}
		s := srv
		if first == nil {
			first = &s
		}
		last := srv.Name
		if i := strings.LastIndexByte(last, '/'); i >= 0 {
			last = last[i+1:]
		}
		if last == serverID || strings.ReplaceAll(last, "_", "-") == want {
			return &s
		}
	}
	return first
}

func registryToConnectSpec(srv *registryServer, sc schema.MCPServerConfig) (connectSpec, *detectedAuth, bool) {
	if srv == nil {
		return connectSpec{}, nil, false
	}
	var npmPkg, pipPkg *registryPackage
	var allEnvNames []string
	for i := range srv.Packages {
		p := &srv.Packages[i]
		for _, ev := range p.envVars() {
			allEnvNames = append(allEnvNames, ev.Name)
		}
		switch strings.ToLower(p.regType()) {
		case "npm":
			if npmPkg == nil {
				npmPkg = p
			}
		case "pip", "pypi":
			if pipPkg == nil {
				pipPkg = p
			}
		}
	}

	var spec connectSpec
	switch {
	case npmPkg != nil:
		spec = connectSpec{Transport: "stdio", Command: "npx", Args: []string{"-y", npmPkg.Identifier},
			Env: mapRegistryEnv(npmPkg.envVars(), sc)}
	case pipPkg != nil:
		spec = connectSpec{Transport: "stdio", Command: "uvx", Args: []string{pipPkg.Identifier},
			Env: mapRegistryEnv(pipPkg.envVars(), sc)}
	default:
		remote := pickRemote(srv.Remotes)
		if remote == nil {
			return connectSpec{}, nil, false
		}
		t := remote.transport()
		switch t {
		case "streamable-http", "streamable_http", "http":
			t = "streamable_http"
		case "sse":
			t = "sse"
		default:
			t = "streamable_http"
		}
		spec = connectSpec{Transport: t, URL: remote.URL, Headers: substituteHeaders(remote.Headers, sc)}
	}
	if spec.Timeout == 0 {
		spec.Timeout = defaultTimeout
	}

	var detected *detectedAuth
	if sc.Auth == nil {
		detected = detectOAuthFromEnvVars(allEnvNames)
		if tokenVar := detectTokenVar(allEnvNames); tokenVar != "" && spec.Env != nil {
			if _, already := spec.Env[tokenVar]; !already {
				if v := tokenishValue(sc.Extra); v != "" {
					spec.Env[tokenVar] = v
				}
			}
		}
	}
	return spec, detected, true
}

func mapRegistryEnv(vars []registryEnvVar, sc schema.MCPServerConfig) map[string]string {
	out := map[string]string{}
	for _, ev := range vars {
		if v, ok := sc.Env[ev.Name]; ok {
			out[ev.Name] = v
			continue
		}
		for _, short := range envVarToShorthands(ev.Name) {
			if v, ok := sc.Extra[short]; ok {
				out[ev.Name] = fmt.Sprintf("%v", v)
				break
			}
		}
	}
	return out
}

func pickRemote(remotes []registryRemote) *registryRemote {
	for i := range remotes {
		if strings.Contains(remotes[i].transport(), "streamable") {
			return &remotes[i]
		}
	}
	if len(remotes) > 0 {
		return &remotes[0]
	}
	return nil
}

func substituteHeaders(headers []registryHeader, sc schema.MCPServerConfig) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, h := range headers {
		val := h.Value
		for k, v := range sc.Extra {
			val = strings.ReplaceAll(val, "{"+k+"}", fmt.Sprintf("%v", v))
		}
		out[h.Name] = val
	}
	return out
}
