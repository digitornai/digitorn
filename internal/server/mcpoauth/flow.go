package mcpoauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/compiler/schema"
)

// providerEndpoints mirrors the Python _WELL_KNOWN_PROVIDERS table verbatim.
type providerEndpoints struct {
	authorizeURL    string
	tokenURL        string
	revokeURL       string
	pkce            bool
	tokenAuthMethod string
	extraAuthorize  map[string]string
}

var wellKnownProviders = map[string]providerEndpoints{
	"google": {
		authorizeURL: "https://accounts.google.com/o/oauth2/v2/auth",
		tokenURL:     "https://oauth2.googleapis.com/token",
		revokeURL:    "https://oauth2.googleapis.com/revoke",
		pkce:         true,
	},
	"github": {
		authorizeURL: "https://github.com/login/oauth/authorize",
		tokenURL:     "https://github.com/login/oauth/access_token",
		pkce:         false,
	},
	"slack": {
		authorizeURL: "https://slack.com/oauth/v2/authorize",
		tokenURL:     "https://slack.com/api/oauth.v2.access",
		revokeURL:    "https://slack.com/api/auth.revoke",
		pkce:         false,
	},
	"microsoft": {
		authorizeURL: "https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		tokenURL:     "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		pkce:         true,
	},
	"notion": {
		authorizeURL:    "https://api.notion.com/v1/oauth/authorize",
		tokenURL:        "https://api.notion.com/v1/oauth/token",
		pkce:            false,
		tokenAuthMethod: "basic",
		extraAuthorize:  map[string]string{"owner": "user"},
	},
}

// resolvedAuth is an MCPAuthConfig merged with the well-known provider table.
type resolvedAuth struct {
	Provider        string
	ClientID        string
	ClientSecret    string
	Scopes          []string
	RedirectURI     string
	AuthorizeURL    string
	TokenURL        string
	RevokeURL       string
	PKCE            bool
	TokenAuthMethod string
	ExtraAuthorize  map[string]string
	EnvTokenVar     string
}

// resolveAuth applies the §5.3 merge: table fills empty URLs; PKCE override fires
// only when the current value is still true (Python `if "pkce" in known and
// self.pkce is True`); token_auth_method override only when current is "body".
func resolveAuth(cfg *schema.MCPAuthConfig) resolvedAuth {
	provider := cfg.Provider
	if provider == "" {
		provider = "custom"
	}
	ra := resolvedAuth{
		Provider:        provider,
		ClientID:        cfg.ClientID,
		ClientSecret:    cfg.ClientSecret,
		Scopes:          cfg.Scopes,
		RedirectURI:     cfg.RedirectURI,
		AuthorizeURL:    cfg.AuthorizeURL,
		TokenURL:        cfg.TokenURL,
		RevokeURL:       cfg.RevokeURL,
		PKCE:            cfg.PKCE == nil || *cfg.PKCE,
		TokenAuthMethod: cfg.TokenAuthMethod,
		EnvTokenVar:     cfg.EnvTokenVar,
		ExtraAuthorize:  stringMap(cfg.ExtraParams),
	}
	if ra.TokenAuthMethod == "" {
		ra.TokenAuthMethod = "body"
	}
	known, ok := wellKnownProviders[provider]
	if !ok {
		return ra
	}
	if ra.AuthorizeURL == "" {
		ra.AuthorizeURL = known.authorizeURL
	}
	if ra.TokenURL == "" {
		ra.TokenURL = known.tokenURL
	}
	if ra.RevokeURL == "" {
		ra.RevokeURL = known.revokeURL
	}
	if ra.PKCE {
		ra.PKCE = known.pkce
	}
	if ra.TokenAuthMethod == "body" && known.tokenAuthMethod != "" {
		ra.TokenAuthMethod = known.tokenAuthMethod
	}
	if len(ra.ExtraAuthorize) == 0 && len(known.extraAuthorize) > 0 {
		ra.ExtraAuthorize = cloneStringMap(known.extraAuthorize)
	}
	return ra
}

// ProviderOf returns the resolved provider id for an auth config ("custom" when
// unset), matching the value tokens are stored under.
func ProviderOf(cfg *schema.MCPAuthConfig) string { return resolveAuth(cfg).Provider }

// generatePKCE produces an S256 verifier/challenge pair. The verifier mirrors
// Python `secrets.token_urlsafe(64)[:128]`; the challenge is the raw-url-safe
// sha256 of it.
func generatePKCE() (verifier, challenge string, err error) {
	b := make([]byte, 64)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", "", err
	}
	verifier = base64.RawURLEncoding.EncodeToString(b)
	if len(verifier) > 128 {
		verifier = verifier[:128]
	}
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

func generateState() (string, error) {
	b := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func buildAuthorizeURL(ra resolvedAuth, state, codeChallenge string) string {
	params := url.Values{}
	params.Set("client_id", ra.ClientID)
	params.Set("redirect_uri", ra.RedirectURI)
	params.Set("response_type", "code")
	params.Set("state", state)
	if len(ra.Scopes) > 0 {
		params.Set("scope", strings.Join(ra.Scopes, " "))
	}
	if ra.PKCE && codeChallenge != "" {
		params.Set("code_challenge", codeChallenge)
		params.Set("code_challenge_method", "S256")
	}
	for k, v := range ra.ExtraAuthorize {
		params.Set(k, v)
	}
	return ra.AuthorizeURL + "?" + params.Encode()
}

// Flow runs the network half of the OAuth dance (code exchange + refresh).
type Flow struct {
	client *http.Client
}

func NewFlow() *Flow {
	return &Flow{client: &http.Client{Timeout: 30 * time.Second}}
}

func (f *Flow) exchange(ctx context.Context, ra resolvedAuth, code, redirectURI, verifier string) (*Token, error) {
	body := map[string]string{
		"grant_type":   "authorization_code",
		"code":         code,
		"redirect_uri": redirectURI,
	}
	if verifier != "" {
		body["code_verifier"] = verifier
	}
	return f.postToken(ctx, ra, body)
}

func (f *Flow) refresh(ctx context.Context, ra resolvedAuth, refreshToken string) (*Token, error) {
	return f.postToken(ctx, ra, map[string]string{
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
}

// refreshIfNeeded refreshes within a 300s buffer of expiry. On any failure it
// returns an error so the caller forces re-auth — never serve a stale token.
func (f *Flow) refreshIfNeeded(ctx context.Context, ra resolvedAuth, tok *Token) (*Token, error) {
	const bufferSeconds = 300
	if tok.ExpiresAt == 0 || time.Now().UTC().Unix() < tok.ExpiresAt-bufferSeconds {
		return tok, nil
	}
	if tok.RefreshToken == "" {
		return nil, fmt.Errorf("mcpoauth: token expired, no refresh_token")
	}
	refreshed, err := f.refresh(ctx, ra, tok.RefreshToken)
	if err != nil {
		return nil, err
	}
	if refreshed.RefreshToken == "" {
		refreshed.RefreshToken = tok.RefreshToken
	}
	return refreshed, nil
}

// postToken sends a token-endpoint request. basic providers carry client creds
// in an HTTP Basic header with a JSON body; body providers inline the creds in a
// form body. Refresh uses the SAME encoding as exchange (fixes the Python
// asymmetry where refresh always sent a form body).
func (f *Flow) postToken(ctx context.Context, ra resolvedAuth, body map[string]string) (*Token, error) {
	var req *http.Request
	var err error
	if ra.TokenAuthMethod == "basic" {
		buf, mErr := json.Marshal(body)
		if mErr != nil {
			return nil, mErr
		}
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, ra.TokenURL, bytes.NewReader(buf))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.SetBasicAuth(ra.ClientID, ra.ClientSecret)
	} else {
		form := url.Values{}
		for k, v := range body {
			form.Set(k, v)
		}
		form.Set("client_id", ra.ClientID)
		form.Set("client_secret", ra.ClientSecret)
		req, err = http.NewRequestWithContext(ctx, http.MethodPost, ra.TokenURL, strings.NewReader(form.Encode()))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcpoauth: token request: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("mcpoauth: token response not JSON (status %d)", resp.StatusCode)
	}
	if e, _ := data["error"].(string); e != "" {
		desc, _ := data["error_description"].(string)
		if desc == "" {
			desc = e
		}
		return nil, fmt.Errorf("mcpoauth: token endpoint error: %s", desc)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("mcpoauth: token endpoint status %d", resp.StatusCode)
	}
	return parseToken(data), nil
}

func parseToken(data map[string]any) *Token {
	tok := &Token{}
	tok.AccessToken, _ = data["access_token"].(string)
	tok.RefreshToken, _ = data["refresh_token"].(string)
	tok.TokenType, _ = data["token_type"].(string)
	tok.Scope, _ = data["scope"].(string)
	if secs := toInt(data["expires_in"]); secs > 0 {
		tok.ExpiresAt = time.Now().UTC().Unix() + int64(secs)
	}
	return tok
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func stringMap(in map[string]any) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = fmt.Sprintf("%v", v)
	}
	return out
}

func cloneStringMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
