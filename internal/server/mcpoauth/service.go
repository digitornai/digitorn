package mcpoauth

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/module/service"
)

// Service is the daemon-side OAuth surface: it owns the encrypted token store,
// the CSRF state store, and the network flow. Handlers resolve the per-server
// auth config and hand it in; the service never reaches into app definitions.
type Service struct {
	tokens     *Store
	states     *StateStore
	flow       *Flow
	serverAuth ServerAuthLookup
	serverURL  ServerURLLookup
	discoverer *discoverer
	clients    *clientStore
	redirectB  string
	// pieceRedirectURL, when set, is the full public OAuth callback used for
	// connector (piece) flows — a hosted bounce page (e.g.
	// https://auth.digitorn.ai/oauth/callback) that forwards the code back to
	// the local daemon. Needed for providers that reject loopback redirects
	// (Slack, Notion…) and to keep one callback per app across desktop + cloud.
	pieceRedirectURL string
}

func NewService(db *gorm.DB, sealer *Sealer) *Service {
	return &Service{
		tokens:     NewStore(db, sealer),
		states:     NewStateStore(db, sealer),
		flow:       NewFlow(),
		discoverer: newDiscoverer(),
		clients:    newClientStore(db, sealer),
	}
}

// Authorize mints an authorization URL and persists the state→user binding. For
// a server declared by URL it first discovers the authorization server and
// dynamically registers a client (enrich), so no per-server client_id is needed.
func (s *Service) Authorize(ctx context.Context, cfg *schema.MCPAuthConfig, userID, appID, serverID string) (authURL, state string, err error) {
	enriched, err := s.enrich(ctx, cfg, appID, serverID)
	if err != nil {
		return "", "", err
	}
	ra := resolveAuth(enriched)
	if ra.AuthorizeURL == "" || ra.ClientID == "" {
		return "", "", fmt.Errorf("mcpoauth: server %q has no usable authorize_url/client_id", serverID)
	}
	state, err = generateState()
	if err != nil {
		return "", "", err
	}
	nonce, err := generateState()
	if err != nil {
		return "", "", err
	}
	var verifier, challenge string
	if ra.PKCE {
		if verifier, challenge, err = generatePKCE(); err != nil {
			return "", "", err
		}
	}
	if err = s.states.Put(ctx, PendingState{
		State:       state,
		UserID:      userID,
		AppID:       appID,
		Provider:    ra.Provider,
		ServerID:    serverID,
		Verifier:    verifier,
		Nonce:       nonce,
		RedirectURI: ra.RedirectURI,
	}); err != nil {
		return "", "", err
	}
	return buildAuthorizeURL(ra, state, challenge), state, nil
}

// AuthorizeForPiece is like Authorize but accepts explicit client credentials
// for pieces that need them (e.g., GitHub, Gmail OAuth2).
func (s *Service) AuthorizeForPiece(ctx context.Context, cfg *schema.MCPAuthConfig, userID, appID, serverID, clientID, clientSecret string) (authURL, state string, err error) {
	// For pieces, we skip enrich and use the config directly.
	ra := resolveAuth(cfg)
	if clientID != "" {
		ra.ClientID = clientID
	}
	if clientSecret != "" {
		ra.ClientSecret = clientSecret
	}
	if ra.AuthorizeURL == "" || ra.ClientID == "" {
		return "", "", fmt.Errorf("mcpoauth: piece %q has no usable authorize_url/client_id", serverID)
	}
	// Generate redirect URI if not set. Prefer the hosted bounce URL when
	// configured (works for loopback-hostile providers + unifies desktop/cloud);
	// otherwise fall back to the daemon's own loopback callback.
	if ra.RedirectURI == "" {
		if s.pieceRedirectURL != "" {
			ra.RedirectURI = s.pieceRedirectURL
		} else {
			ra.RedirectURI = s.redirectB + "/api/oauth/mcp/callback"
		}
	}
	state, err = generateState()
	if err != nil {
		return "", "", err
	}
	nonce, err := generateState()
	if err != nil {
		return "", "", err
	}
	var verifier, challenge string
	if ra.PKCE {
		if verifier, challenge, err = generatePKCE(); err != nil {
			return "", "", err
		}
	}
	if err = s.states.Put(ctx, PendingState{
		State:        state,
		UserID:       userID,
		AppID:        appID,
		Provider:     ra.Provider,
		ServerID:     serverID,
		Verifier:     verifier,
		Nonce:        nonce,
		RedirectURI:  ra.RedirectURI,
		ClientID:     clientID,
		ClientSecret: clientSecret,
	}); err != nil {
		return "", "", err
	}
	return buildAuthorizeURL(ra, state, challenge), state, nil
}

// TakeState consumes a pending state (single-use). Returns nil for unknown/expired.
func (s *Service) TakeState(ctx context.Context, state string) (*PendingState, error) {
	return s.states.TakeValid(ctx, state)
}

// Exchange swaps the authorization code for a token (using the state's verifier)
// and stores it encrypted for (user, provider).
func (s *Service) Exchange(ctx context.Context, cfg *schema.MCPAuthConfig, p *PendingState, code string) error {
	enriched, err := s.enrich(ctx, cfg, p.AppID, p.ServerID)
	if err != nil {
		return err
	}
	ra := resolveAuth(enriched)
	tok, err := s.flow.exchange(ctx, ra, code, p.RedirectURI, p.Verifier)
	if err != nil {
		return err
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("mcpoauth: token endpoint returned no access_token")
	}
	return s.tokens.Set(ctx, p.UserID, p.Provider, tok)
}

// ExchangeForPiece swaps the authorization code for a token using the piece's
// config directly — no app enrich (mirrors AuthorizeForPiece, which also skips
// it). Returns the token so the caller can store it in the pieces store; does
// not touch the generic per-provider token store (pieces are keyed by
// user+piece there, so there's no provider-key collision).
func (s *Service) ExchangeForPiece(ctx context.Context, cfg *schema.MCPAuthConfig, p *PendingState, code string) (*Token, error) {
	ra := resolveAuth(cfg)
	tok, err := s.flow.exchange(ctx, ra, code, p.RedirectURI, p.Verifier)
	if err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, fmt.Errorf("mcpoauth: token endpoint returned no access_token")
	}
	return tok, nil
}

// SetManual stores a token supplied directly by the client (bypassing the flow).
func (s *Service) SetManual(ctx context.Context, userID, provider string, tok *Token) error {
	return s.tokens.Set(ctx, userID, provider, tok)
}

// GetToken returns the token for (userID, provider), or nil when none exists.
func (s *Service) GetToken(ctx context.Context, userID, provider string) (*Token, error) {
	return s.tokens.Get(ctx, userID, provider)
}

// Revoke best-effort revokes the token at the provider (RFC 7009), then deletes
// it locally. Scoped to (user, provider) — never touches other users. The local
// delete always runs even if the provider revoke fails.
func (s *Service) Revoke(ctx context.Context, cfg *schema.MCPAuthConfig, userID, appID, serverID string) error {
	enriched, err := s.enrich(ctx, cfg, appID, serverID)
	if err != nil {
		enriched = cfg // best-effort: still delete the local token under the static key
	}
	ra := resolveAuth(enriched)
	if tok, err := s.tokens.Get(ctx, userID, ra.Provider); err == nil && tok != nil && tok.AccessToken != "" {
		_ = s.flow.revoke(ctx, ra, tok.AccessToken)
	}
	return s.tokens.Delete(ctx, userID, ra.Provider)
}

// ListingAuthContext returns a credential suitable for CONNECTING an OAuth server
// to LIST its tools at materialization time. Tool specs are user-independent, so
// it uses ANY user's stored token for the server's provider (refreshing if
// needed). Returns nil when the server isn't oauth2 or nobody has authorized yet —
// then the server simply contributes no tools until someone does. The per-user
// token is still required (and resolved) at INVOKE time.
func (s *Service) ListingAuthContext(ctx context.Context, cfg *schema.MCPAuthConfig, appID, serverID string) *service.AuthContext {
	if cfg == nil || cfg.Type != "oauth2" {
		return nil
	}
	enriched, err := s.enrich(ctx, cfg, appID, serverID)
	if err != nil {
		return nil
	}
	ra := resolveAuth(enriched)
	tok, userID, err := s.tokens.AnyForProvider(ctx, ra.Provider)
	if err != nil || tok == nil || tok.AccessToken == "" {
		return nil
	}
	fresh, rerr := s.flow.refreshIfNeeded(ctx, ra, tok)
	if rerr != nil || fresh == nil {
		return nil
	}
	if fresh != tok && userID != "" {
		_ = s.tokens.Set(ctx, userID, ra.Provider, fresh)
	}
	return &service.AuthContext{
		Token:        fresh.AccessToken,
		TokenType:    tokenTypeOr(fresh.TokenType),
		EnvTokenVar:  ra.EnvTokenVar,
		ExpiresAt:    fresh.ExpiresAt,
		Provider:     ra.Provider,
		RefreshToken: fresh.RefreshToken,
		Scope:        fresh.Scope,
		ClientID:     ra.ClientID,
		ClientSecret: ra.ClientSecret,
	}
}

// HasValidToken reports whether (user, provider) holds a present, non-expired
// token (expiry judged with the same 300s buffer used at resolve time).
func (s *Service) HasValidToken(ctx context.Context, userID, provider string) bool {
	tok, err := s.tokens.Get(ctx, userID, provider)
	if err != nil || tok == nil || tok.AccessToken == "" {
		return false
	}
	if tok.ExpiresAt == 0 {
		return true
	}
	if time.Now().UTC().Unix() >= tok.ExpiresAt-300 {
		return tok.RefreshToken != "" // refreshable counts as valid
	}
	return true
}
