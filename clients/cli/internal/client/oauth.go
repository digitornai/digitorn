package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// DefaultAuthIssuer is the canonical auth server URL. Overridable per-call
// via OAuthConfig.Issuer or by the DIGITORN_AUTH_URL env var.
const DefaultAuthIssuer = "https://auth.digitorn.ai"

// DefaultProvider is the upstream identity provider the auth server bounces
// the user to. Options : google | microsoft | azure.
const DefaultProvider = "google"

// OAuthConfig configures the login flow. All fields have defaults.
//
// The flow this implements is the digitorn-bridge (Python daemon) browser
// bounce flow — NOT standard OAuth2 + PKCE. The auth server bounces the
// user through Google/Microsoft/Azure then redirects the browser to a
// localhost listener with the access/refresh tokens in the query string.
type OAuthConfig struct {
	Issuer   string
	Provider string

	// PromptUser is called with the authorize URL after the browser open
	// is attempted. Lets the caller print a fallback message. Optional.
	PromptUser func(authorizeURL string)

	// Timeout caps the user's window to complete the browser dance. The
	// local callback listener is closed when this elapses. Defaults to 3m.
	Timeout time.Duration
}

// Login runs the digitorn-auth browser bounce flow synchronously. The CLI :
//
//  1. Binds a one-shot HTTP listener on 127.0.0.1:<random>.
//  2. Opens the system browser to
//     https://auth.digitorn.ai/auth/oauth/{provider}?bounce_to=<callback>
//  3. The auth server signs the user in via the upstream provider
//     (Google/Microsoft/Azure), then redirects the browser to the callback
//     URL with the tokens in the query string.
//  4. The listener captures the tokens and returns them as Credentials.
//
// Returns Credentials ready to persist via SaveCredentials.
func Login(ctx context.Context, cfg OAuthConfig) (*Credentials, error) {
	cfg = cfg.withDefaults()
	if cfg.Provider != "google" && cfg.Provider != "microsoft" && cfg.Provider != "azure" {
		return nil, fmt.Errorf("oauth: unknown provider %q (valid: google | microsoft | azure)", cfg.Provider)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth: bind local listener: %w", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	callbackURL := fmt.Sprintf("http://127.0.0.1:%d/oauth-callback", port)

	authorizeURL := fmt.Sprintf("%s/auth/oauth/%s?bounce_to=%s",
		strings.TrimRight(cfg.Issuer, "/"),
		cfg.Provider,
		url.QueryEscape(callbackURL),
	)

	if err := openBrowser(authorizeURL); err != nil {
		// Browser open is best-effort. If it fails (headless, missing tool),
		// fall back to the prompt — the user can paste the URL manually.
		if cfg.PromptUser != nil {
			cfg.PromptUser(authorizeURL)
		}
	} else if cfg.PromptUser != nil {
		cfg.PromptUser(authorizeURL)
	}

	tokens, err := waitForBounce(ctx, ln, cfg.Timeout)
	if err != nil {
		return nil, err
	}

	creds := &Credentials{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		AuthURL:      cfg.Issuer,
		Provider:     firstNonEmpty(tokens.Provider, cfg.Provider),
	}
	if tokens.ExpiresIn > 0 {
		creds.ExpiresAt = float64(time.Now().Add(time.Duration(tokens.ExpiresIn) * time.Second).Unix())
	}
	// Decode the JWT (best-effort, no signature verify) to fill the
	// human-readable fields for the status bar. The daemon (T6) does the
	// actual signature check before trusting the token.
	if claims := decodeJWTClaims(tokens.AccessToken); claims != nil {
		creds.UserID = firstNonEmpty(claims.UserID, claims.Sub)
		creds.Email = claims.Email
		creds.Name = firstNonEmpty(claims.Name, claims.DisplayName)
	}
	if creds.UserID == "" {
		creds.UserID = creds.Email
	}
	return creds, nil
}

// RefreshAccessToken trades a refresh_token for fresh tokens by POSTing to
// /auth/refresh. The auth server returns the same TokenResponse shape the
// initial login does.
func RefreshAccessToken(ctx context.Context, cfg OAuthConfig, refreshToken string) (*Credentials, error) {
	cfg = cfg.withDefaults()
	if refreshToken == "" {
		return nil, errors.New("oauth: no refresh_token available, run `digitorn login` again")
	}

	body, err := json.Marshal(map[string]string{"refresh_token": refreshToken})
	if err != nil {
		return nil, fmt.Errorf("oauth: encode refresh: %w", err)
	}
	endpoint := strings.TrimRight(cfg.Issuer, "/") + "/auth/refresh"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	httpClient := &http.Client{Timeout: 10 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: refresh request: %w", err)
	}
	defer resp.Body.Close()
	rawBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: refresh rejected (HTTP %d): %s", resp.StatusCode, truncString(string(rawBody), 200))
	}

	var t struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token,omitempty"`
		ExpiresIn    int    `json:"expires_in"`
		Email        string `json:"email,omitempty"`
		UserID       string `json:"user_id,omitempty"`
		DisplayName  string `json:"display_name,omitempty"`
	}
	if err := json.Unmarshal(rawBody, &t); err != nil {
		return nil, fmt.Errorf("oauth: decode refresh: %w", err)
	}
	if t.AccessToken == "" {
		return nil, errors.New("oauth: refresh response had empty access_token")
	}
	creds := &Credentials{
		AccessToken:  t.AccessToken,
		RefreshToken: firstNonEmpty(t.RefreshToken, refreshToken),
		AuthURL:      cfg.Issuer,
		Provider:     "oauth",
		UserID:       t.UserID,
		Email:        t.Email,
		Name:         t.DisplayName,
	}
	if t.ExpiresIn > 0 {
		creds.ExpiresAt = float64(time.Now().Add(time.Duration(t.ExpiresIn) * time.Second).Unix())
	}
	return creds, nil
}

func (c OAuthConfig) withDefaults() OAuthConfig {
	if c.Issuer == "" {
		c.Issuer = DefaultAuthIssuer
	}
	c.Issuer = strings.TrimRight(c.Issuer, "/")
	if c.Provider == "" {
		c.Provider = DefaultProvider
	}
	if c.Timeout <= 0 {
		c.Timeout = 3 * time.Minute
	}
	return c
}

// bounceTokens is the data the auth server pushes onto our callback URL's
// query string after the upstream OAuth dance completes.
type bounceTokens struct {
	AccessToken  string
	RefreshToken string
	ExpiresIn    int
	Provider     string
}

// waitForBounce runs a minimal HTTP server on the bound listener that only
// serves a single /oauth-callback request, then shuts down. Returns the
// parsed tokens or an error.
func waitForBounce(ctx context.Context, ln net.Listener, timeout time.Duration) (*bounceTokens, error) {
	type result struct {
		tokens *bounceTokens
		err    error
	}
	resCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("oauth_error"); e != "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, callbackErrorHTML(e))
			resCh <- result{err: fmt.Errorf("oauth provider error: %s", e)}
			return
		}
		access := q.Get("access_token")
		if access == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, callbackErrorHTML("missing access_token"))
			resCh <- result{err: errors.New("oauth callback: missing access_token in bounce query")}
			return
		}
		tokens := &bounceTokens{
			AccessToken:  access,
			RefreshToken: q.Get("refresh_token"),
			Provider:     q.Get("provider"),
		}
		if v := q.Get("expires_in"); v != "" {
			if n, err := strconv.Atoi(v); err == nil {
				tokens.ExpiresIn = n
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, callbackSuccessHTML())
		resCh <- result{tokens: tokens}
	})

	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	defer func() {
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancel()
	}()

	select {
	case r := <-resCh:
		return r.tokens, r.err
	case <-ctx.Done():
		return nil, fmt.Errorf("oauth: %w", ctx.Err())
	case <-time.After(timeout):
		return nil, fmt.Errorf("oauth: timed out after %s waiting for browser callback", timeout)
	}
}

// --- JWT claim decode (no signature verify) -----------------------------

type jwtClaims struct {
	Sub         string `json:"sub"`
	UserID      string `json:"user_id"`
	Email       string `json:"email"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
}

func decodeJWTClaims(token string) *jwtClaims {
	if token == "" {
		return nil
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some implementations use standard base64 with padding.
		payload, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return nil
		}
	}
	var c jwtClaims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil
	}
	return &c
}

// --- Browser open --------------------------------------------------------

// openBrowser launches the system browser at the target URL. Cross-platform.
// Returns the spawn error ; the browser process keeps running after.
func openBrowser(target string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 is more reliable than `start` (which has cmd quoting hell).
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	case "darwin":
		cmd = exec.Command("open", target)
	default:
		cmd = exec.Command("xdg-open", target)
	}
	return cmd.Start()
}

// --- HTML responses shown in the browser at the end of the flow ---------

func callbackSuccessHTML() string {
	return `<!doctype html>
<html><head><meta charset="utf-8"><title>digitorn — signed in</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#0a0a0c;color:#e8e8ea;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#16161a;border:1px solid #2a2a30;border-radius:12px;padding:32px 40px;text-align:center;max-width:420px}
h1{font-size:20px;margin:0 0 8px;color:#fff}
p{margin:0;color:#9a9aa3;font-size:14px;line-height:1.5}
.ok{color:#4ade80;font-size:32px;margin-bottom:12px}
</style></head>
<body><div class="card">
<div class="ok">✓</div>
<h1>You're signed in</h1>
<p>You can close this tab and return to your terminal.</p>
</div></body></html>`
}

func callbackErrorHTML(msg string) string {
	return fmt.Sprintf(`<!doctype html>
<html><head><meta charset="utf-8"><title>digitorn — sign-in failed</title>
<style>
body{font-family:-apple-system,BlinkMacSystemFont,sans-serif;background:#0a0a0c;color:#e8e8ea;display:flex;align-items:center;justify-content:center;height:100vh;margin:0}
.card{background:#16161a;border:1px solid #ff4e4e;border-radius:12px;padding:32px 40px;text-align:center;max-width:420px}
h1{font-size:20px;margin:0 0 8px;color:#fff}
p{margin:0;color:#9a9aa3;font-size:14px;line-height:1.5}
code{background:#0a0a0c;padding:2px 6px;border-radius:4px;font-size:12px}
.err{color:#ff4e4e;font-size:32px;margin-bottom:12px}
</style></head>
<body><div class="card">
<div class="err">✗</div>
<h1>Sign-in failed</h1>
<p><code>%s</code></p>
</div></body></html>`, htmlEscape(msg))
}

// --- tiny helpers --------------------------------------------------------

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncString(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return r.Replace(s)
}
