package mcpoauth

import (
	"context"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/internal/module/service"
)

// ServerAuthLookup resolves a server's oauth2 auth block for an app. The daemon
// wires it to read the live app definition; returns nil for unknown/non-oauth.
type ServerAuthLookup func(appID, serverID string) *schema.MCPAuthConfig

func (s *Service) SetServerAuthLookup(fn ServerAuthLookup) { s.serverAuth = fn }

// ResolveAuth implements service.AuthResolver. For an MCP tool whose server has
// an oauth2 block it returns a fresh AuthContext (refreshing if needed), or an
// AuthChallenge when no usable token exists. For everything else it returns
// (nil, nil, nil) — no credential required.
func (s *Service) ResolveAuth(ctx context.Context, userID, appID, moduleID, toolName string) (*service.AuthContext, *service.AuthChallenge, error) {
	if moduleID != "mcp" || userID == "" || s.serverAuth == nil {
		return nil, nil, nil
	}
	serverID := serverFromTool(toolName)
	if serverID == "" {
		return nil, nil, nil
	}
	cfg := s.serverAuth(appID, serverID)
	if cfg == nil || cfg.Type != "oauth2" {
		return nil, nil, nil
	}
	ra := resolveAuth(cfg)
	provider := ra.Provider

	tok, err := s.tokens.Get(ctx, userID, provider)
	if err != nil {
		return nil, nil, err
	}
	if tok != nil && tok.AccessToken != "" {
		fresh, rerr := s.flow.refreshIfNeeded(ctx, ra, tok)
		if rerr == nil && fresh != nil {
			// Persist whenever a refresh actually happened (refreshIfNeeded returns
			// the SAME pointer when still valid, a NEW one when refreshed) — even if
			// the access_token is unchanged but the expiry moved. Best-effort: a
			// failed write self-heals on the next call's refresh.
			if fresh != tok {
				_ = s.tokens.Set(ctx, userID, provider, fresh)
			}
			return &service.AuthContext{
				Token:       fresh.AccessToken,
				TokenType:   tokenTypeOr(fresh.TokenType),
				EnvTokenVar: ra.EnvTokenVar,
				ExpiresAt:   fresh.ExpiresAt,
			}, nil, nil
		}
		// refresh failed → fall through to a fresh challenge (never serve stale)
	}

	authURL, state, aerr := s.Authorize(ctx, cfg, userID, appID, serverID)
	if aerr != nil {
		return nil, nil, aerr
	}
	return nil, &service.AuthChallenge{
		Provider: provider, ServerID: serverID, AuthURL: authURL, State: state,
	}, nil
}

func tokenTypeOr(t string) string {
	if t == "" {
		return "Bearer"
	}
	return t
}

// serverFromTool extracts "<server>" from "mcp_<server>__<tool>" (server names may
// contain underscores; the split is on the first "__").
func serverFromTool(name string) string {
	i := strings.Index(name, "__")
	if i < 0 {
		return ""
	}
	return strings.TrimPrefix(name[:i], "mcp_")
}
