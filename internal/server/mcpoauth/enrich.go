package mcpoauth

import (
	"context"
	"net/url"
	"strings"

	"github.com/digitornai/digitorn/internal/compiler/schema"
)

// discoveryCallbackPath is the app-agnostic OAuth callback for the discovery
// flow. The pending state carries app+server, so one redirect URI serves every
// app — which is also the single URI a dynamically-registered client is bound to.
const discoveryCallbackPath = "/api/oauth/mcp/callback"

// ServerURLLookup resolves a server's remote URL for an app (empty for stdio or
// unknown servers). Used to discover an authorization server when the auth block
// carries no explicit endpoints/client.
type ServerURLLookup func(appID, serverID string) string

type ctxKey int

const ctxServerURLKey ctxKey = iota

// WithServerURL pins a server's remote URL on the context, used by callers
// (e.g. per-user managed servers) whose URL the app-keyed ServerURLLookup can't
// resolve. enrich prefers this over the lookup.
func WithServerURL(ctx context.Context, rawURL string) context.Context {
	return context.WithValue(ctx, ctxServerURLKey, rawURL)
}

func serverURLFromCtx(ctx context.Context) string {
	s, _ := ctx.Value(ctxServerURLKey).(string)
	return s
}

// SetServerURLLookup wires the server-URL resolver used by discovery.
func (s *Service) SetServerURLLookup(fn ServerURLLookup) { s.serverURL = fn }

// SetRedirectBase sets the daemon's externally-reachable base URL (scheme+host),
// used to build the OAuth callback redirect URI for discovered servers.
func (s *Service) SetRedirectBase(base string) { s.redirectB = strings.TrimRight(base, "/") }

// SetPieceRedirectURL sets the full hosted OAuth callback used for connector
// flows (the bounce page that forwards the code to the local daemon). Empty =
// fall back to the daemon's loopback callback.
func (s *Service) SetPieceRedirectURL(u string) { s.pieceRedirectURL = strings.TrimRight(u, "/") }

func (s *Service) discoveryRedirectURI() string { return s.redirectB + discoveryCallbackPath }

// enrich fills an oauth2 auth block that lacks explicit endpoints/client by
// discovering the server's authorization server (RFC 9728 → RFC 8414) and
// dynamically registering a client (RFC 7591). A block that already resolves to
// an authorize URL + client_id is returned unchanged (the static path). The
// caller's config is never mutated.
func (s *Service) enrich(ctx context.Context, cfg *schema.MCPAuthConfig, appID, serverID string) (*schema.MCPAuthConfig, error) {
	if ra := resolveAuth(cfg); ra.AuthorizeURL != "" && ra.ClientID != "" {
		return cfg, nil // static / table-known with a client — no network needed
	}
	if s.discoverer == nil || s.clients == nil {
		return cfg, nil
	}
	serverURL := serverURLFromCtx(ctx)
	if serverURL == "" && s.serverURL != nil {
		serverURL = s.serverURL(appID, serverID)
	}
	if serverURL == "" {
		return cfg, nil // nothing to discover from (e.g. stdio) — leave as-is
	}

	disc, err := s.discoverer.discover(ctx, serverURL)
	if err != nil {
		return nil, err
	}
	redirectURI := cfg.RedirectURI
	if redirectURI == "" {
		redirectURI = s.discoveryRedirectURI()
	}
	scope := scopeString(cfg.Scopes, disc)
	client, err := s.clients.getOrRegister(ctx, disc.meta, "Digitorn", redirectURI, scope)
	if err != nil {
		return nil, err
	}

	out := *cfg
	out.Provider = providerKey(cfg.Provider, disc.meta.Issuer)
	out.AuthorizeURL = disc.meta.AuthorizationEndpoint
	out.TokenURL = disc.meta.TokenEndpoint
	if out.Resource == "" {
		out.Resource = disc.resource
	}
	if out.RevokeURL == "" {
		out.RevokeURL = disc.meta.RevocationEndpoint
	}
	out.ClientID = client.ClientID
	out.ClientSecret = client.ClientSecret
	out.RedirectURI = redirectURI
	if len(out.Scopes) == 0 && scope != "" {
		out.Scopes = strings.Fields(scope)
	}
	pkce := disc.meta.supportsS256()
	out.PKCE = &pkce
	if client.ClientSecret == "" {
		out.TokenAuthMethod = "none" // public client — PKCE only, no secret on the token request
	}
	return &out, nil
}

// ProviderKeyResolved returns the token-store key for a server, running discovery
// when the block carries no explicit provider/endpoints. Cheap after the first
// call (discovery + registration are cached/persisted). Falls back to the static
// provider on any discovery error.
func (s *Service) ProviderKeyResolved(ctx context.Context, cfg *schema.MCPAuthConfig, appID, serverID string) string {
	enriched, err := s.enrich(ctx, cfg, appID, serverID)
	if err != nil {
		return ProviderOf(cfg)
	}
	return resolveAuth(enriched).Provider
}

// providerKey is the stable token-store key: the user-configured provider when
// set, else a per-authorization-server key derived from the issuer host.
func providerKey(configured, issuer string) string {
	if configured != "" {
		return configured
	}
	if h := hostOf(issuer); h != "" {
		return "mcp:" + h
	}
	return "custom"
}

// scopeString picks the scopes to request: the user's explicit scopes, else the
// scopes the protected resource advertises. We deliberately do NOT request every
// scope the AS supports (that often forces admin consent) — empty lets the AS
// apply its default.
func scopeString(configured []string, disc discoveredAuth) string {
	if len(configured) > 0 {
		return strings.Join(configured, " ")
	}
	if len(disc.resourceScopes) > 0 {
		return strings.Join(disc.resourceScopes, " ")
	}
	return ""
}

func hostOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Host
}
