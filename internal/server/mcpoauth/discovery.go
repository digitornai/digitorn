package mcpoauth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

// authServerMetadata is the subset of an OAuth 2.0 Authorization Server Metadata
// document (RFC 8414, or the OIDC discovery document) that the flow consumes.
type authServerMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	RevocationEndpoint            string   `json:"revocation_endpoint"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
	ScopesSupported               []string `json:"scopes_supported"`
	TokenEndpointAuthMethods      []string `json:"token_endpoint_auth_methods_supported"`
}

func (m authServerMetadata) supportsS256() bool {
	for _, x := range m.CodeChallengeMethodsSupported {
		if strings.EqualFold(x, "S256") {
			return true
		}
	}
	return false
}

// protectedResourceMetadata is the subset of RFC 9728 metadata we read off a
// protected MCP resource: which authorization server(s) guard it.
type protectedResourceMetadata struct {
	Resource             string   `json:"resource"`
	AuthorizationServers []string `json:"authorization_servers"`
	ScopesSupported      []string `json:"scopes_supported"`
}

// discoveredAuth is the result of discovery for one MCP server URL.
type discoveredAuth struct {
	meta           authServerMetadata
	resourceScopes []string
	resource       string // RFC 8707 resource indicator (the server's canonical URI)
}

// wwwAuthResourceRe pulls the resource_metadata URL out of a 401's
// WWW-Authenticate header (RFC 9728 §5.1): `Bearer resource_metadata="https://…"`.
var wwwAuthResourceRe = regexp.MustCompile(`(?i)resource_metadata\s*=\s*"([^"]+)"`)

// parseWWWAuthenticate returns the resource_metadata URL advertised in a
// WWW-Authenticate header, or "" when absent.
func parseWWWAuthenticate(header string) string {
	if m := wwwAuthResourceRe.FindStringSubmatch(header); m != nil {
		return m[1]
	}
	return ""
}

// discoverer performs RFC 9728 + RFC 8414/OIDC discovery, cached per server URL.
type discoverer struct {
	client *http.Client
	ttl    time.Duration
	mu     sync.Mutex
	cache  map[string]discoCacheEntry
}

type discoCacheEntry struct {
	auth discoveredAuth
	at   time.Time
}

func newDiscoverer() *discoverer {
	return &discoverer{
		client: &http.Client{Timeout: 15 * time.Second},
		ttl:    time.Hour,
		cache:  map[string]discoCacheEntry{},
	}
}

// discover resolves a remote MCP server URL to its authorization server's
// metadata: probe → WWW-Authenticate / RFC 9728 protected-resource metadata →
// RFC 8414 authorization-server metadata (OIDC fallback). Cached per server URL.
func (d *discoverer) discover(ctx context.Context, serverURL string) (discoveredAuth, error) {
	d.mu.Lock()
	if e, ok := d.cache[serverURL]; ok && time.Since(e.at) < d.ttl {
		d.mu.Unlock()
		return e.auth, nil
	}
	d.mu.Unlock()

	prm, prmURL := d.protectedResource(ctx, serverURL)
	issuer := ""
	var resourceScopes []string
	resource := ""
	if prm != nil {
		resourceScopes = prm.ScopesSupported
		resource = strings.TrimSpace(prm.Resource)
		if len(prm.AuthorizationServers) > 0 {
			issuer = strings.TrimSpace(prm.AuthorizationServers[0])
		}
	}
	if resource == "" {
		resource = serverURL // fall back to the server URL as the resource indicator
	}
	if issuer == "" {
		// No usable protected-resource metadata — fall back to the server's own
		// origin as the issuer (servers that co-locate AS metadata there).
		issuer = originOf(serverURL)
	}
	if issuer == "" {
		return discoveredAuth{}, fmt.Errorf("mcpoauth: no authorization server discoverable for %q (prm=%q)", serverURL, prmURL)
	}

	meta, err := d.authServer(ctx, issuer)
	if err != nil {
		return discoveredAuth{}, err
	}
	if meta.Issuer == "" {
		meta.Issuer = issuer
	}
	out := discoveredAuth{meta: meta, resourceScopes: resourceScopes, resource: resource}

	d.mu.Lock()
	d.cache[serverURL] = discoCacheEntry{auth: out, at: time.Now()}
	d.mu.Unlock()
	return out, nil
}

// protectedResource fetches RFC 9728 metadata: first by probing the server for a
// 401 WWW-Authenticate resource_metadata pointer, then the conventional
// well-known locations.
func (d *discoverer) protectedResource(ctx context.Context, serverURL string) (*protectedResourceMetadata, string) {
	if rmURL := d.probeResourceMetadataURL(ctx, serverURL); rmURL != "" {
		if prm := d.fetchPRM(ctx, rmURL); prm != nil {
			return prm, rmURL
		}
	}
	for _, u := range wellKnownURLs(serverURL, "oauth-protected-resource") {
		if prm := d.fetchPRM(ctx, u); prm != nil {
			return prm, u
		}
	}
	return nil, ""
}

// probeResourceMetadataURL sends a minimal MCP request; an auth-gated server
// answers 401 with a WWW-Authenticate header pointing at its resource metadata.
func (d *discoverer) probeResourceMetadataURL(ctx context.Context, serverURL string) string {
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, serverURL, body)
	if err != nil {
		return ""
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := d.client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode == http.StatusUnauthorized {
		return parseWWWAuthenticate(resp.Header.Get("WWW-Authenticate"))
	}
	return ""
}

func (d *discoverer) fetchPRM(ctx context.Context, u string) *protectedResourceMetadata {
	var prm protectedResourceMetadata
	if err := d.getJSON(ctx, u, &prm); err != nil {
		return nil
	}
	if len(prm.AuthorizationServers) == 0 && prm.Resource == "" {
		return nil
	}
	return &prm
}

// authServer fetches RFC 8414 metadata (OIDC discovery as a fallback), trying the
// path-aware and origin well-known locations in turn.
func (d *discoverer) authServer(ctx context.Context, issuer string) (authServerMetadata, error) {
	var firstErr error
	for _, suffix := range []string{"oauth-authorization-server", "openid-configuration"} {
		for _, u := range wellKnownURLs(issuer, suffix) {
			var meta authServerMetadata
			if err := d.getJSON(ctx, u, &meta); err != nil {
				if firstErr == nil {
					firstErr = err
				}
				continue
			}
			if meta.AuthorizationEndpoint != "" && meta.TokenEndpoint != "" {
				return meta, nil
			}
		}
	}
	if firstErr == nil {
		firstErr = fmt.Errorf("mcpoauth: authorization server %q exposes no usable metadata", issuer)
	}
	return authServerMetadata{}, firstErr
}

func (d *discoverer) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := d.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mcpoauth: GET %s: status %d", u, resp.StatusCode)
	}
	return json.Unmarshal(raw, out)
}

// wellKnownURLs builds the candidate metadata URLs for an issuer/resource and a
// well-known suffix, honoring RFC 8414's path-aware form (well-known inserted
// between host and path) before the plain origin form.
func wellKnownURLs(raw, suffix string) []string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil
	}
	var out []string
	if p := strings.TrimSuffix(u.Path, "/"); p != "" {
		out = append(out, fmt.Sprintf("%s://%s/.well-known/%s%s", u.Scheme, u.Host, suffix, p))
	}
	out = append(out, fmt.Sprintf("%s://%s/.well-known/%s", u.Scheme, u.Host, suffix))
	return out
}

func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
