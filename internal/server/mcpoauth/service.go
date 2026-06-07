package mcpoauth

import (
	"context"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// Service is the daemon-side OAuth surface: it owns the encrypted token store,
// the CSRF state store, and the network flow. Handlers resolve the per-server
// auth config and hand it in; the service never reaches into app definitions.
type Service struct {
	tokens     *Store
	states     *StateStore
	flow       *Flow
	serverAuth ServerAuthLookup
}

func NewService(db *gorm.DB, sealer *Sealer) *Service {
	return &Service{
		tokens: NewStore(db, sealer),
		states: NewStateStore(db, sealer),
		flow:   NewFlow(),
	}
}

// Authorize mints an authorization URL and persists the state→user binding.
func (s *Service) Authorize(ctx context.Context, cfg *schema.MCPAuthConfig, userID, appID, serverID string) (authURL, state string, err error) {
	ra := resolveAuth(cfg)
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

// TakeState consumes a pending state (single-use). Returns nil for unknown/expired.
func (s *Service) TakeState(ctx context.Context, state string) (*PendingState, error) {
	return s.states.TakeValid(ctx, state)
}

// Exchange swaps the authorization code for a token (using the state's verifier)
// and stores it encrypted for (user, provider).
func (s *Service) Exchange(ctx context.Context, cfg *schema.MCPAuthConfig, p *PendingState, code string) error {
	ra := resolveAuth(cfg)
	tok, err := s.flow.exchange(ctx, ra, code, p.RedirectURI, p.Verifier)
	if err != nil {
		return err
	}
	if tok.AccessToken == "" {
		return fmt.Errorf("mcpoauth: token endpoint returned no access_token")
	}
	return s.tokens.Set(ctx, p.UserID, p.Provider, tok)
}

// SetManual stores a token supplied directly by the client (bypassing the flow).
func (s *Service) SetManual(ctx context.Context, userID, provider string, tok *Token) error {
	return s.tokens.Set(ctx, userID, provider, tok)
}

// Revoke best-effort revokes the token at the provider (RFC 7009), then deletes
// it locally. Scoped to (user, provider) — never touches other users. The local
// delete always runs even if the provider revoke fails.
func (s *Service) Revoke(ctx context.Context, cfg *schema.MCPAuthConfig, userID string) error {
	ra := resolveAuth(cfg)
	if tok, err := s.tokens.Get(ctx, userID, ra.Provider); err == nil && tok != nil && tok.AccessToken != "" {
		_ = s.flow.revoke(ctx, ra, tok.AccessToken)
	}
	return s.tokens.Delete(ctx, userID, ra.Provider)
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
