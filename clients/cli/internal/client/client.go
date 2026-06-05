package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DefaultTimeout caps each HTTP request. Streaming endpoints
// (Socket.IO) are handled in a separate package with their own
// timeouts ; this only applies to one-shot REST calls.
const DefaultTimeout = 30 * time.Second

// Options bundles construction-time settings. Zero values trigger
// safe defaults : DefaultTimeout, http.DefaultTransport, no bearer.
type Options struct {
	// BaseURL is the daemon's HTTP endpoint, e.g. "http://127.0.0.1:28002".
	// Required. Use Discover() to compute it from env + config + defaults.
	BaseURL string

	// BearerToken is the JWT forwarded as Authorization. Empty = no
	// Authorization header sent (acceptable in dev-mode daemon).
	BearerToken string

	// UserID is sent as the X-User-ID header (dev-mode user pinning).
	// Empty = anonymous.
	UserID string

	// HTTPClient lets tests inject a mock. nil = an http.Client with
	// DefaultTimeout.
	HTTPClient *http.Client

	// RefreshToken + AuthURL enable transparent token refresh : when a
	// request gets a 401, the client trades the refresh token for a fresh
	// access token (against AuthURL/auth/refresh) and replays the request
	// once. Empty RefreshToken disables this (a 401 propagates as-is).
	RefreshToken string
	AuthURL      string

	// OnTokenRefresh, if set, is invoked with the fresh credentials right
	// after a successful in-flight refresh — so the caller can persist
	// them and propagate the new token (e.g. to the realtime socket).
	OnTokenRefresh func(*Credentials)
}

// Client is the typed REST wrapper. Every method maps to one daemon
// endpoint. Methods are safe for concurrent use ; the underlying
// http.Client is shared.
type Client struct {
	baseURL    *url.URL
	userID     string
	httpClient *http.Client

	// mu guards the mutable auth state (bearer + refreshToken), which a
	// 401-triggered refresh rewrites mid-flight.
	mu           sync.Mutex
	bearer       string
	refreshToken string
	authURL      string
	onRefresh    func(*Credentials)
	refreshing   bool
}

// New constructs a Client. Returns an error if BaseURL is missing or
// malformed.
func New(opts Options) (*Client, error) {
	if opts.BaseURL == "" {
		return nil, errors.New("client: BaseURL required (try client.Discover())")
	}
	u, err := url.Parse(opts.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("client: invalid BaseURL %q: %w", opts.BaseURL, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("client: BaseURL must be absolute, got %q", opts.BaseURL)
	}
	httpC := opts.HTTPClient
	if httpC == nil {
		httpC = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{
		baseURL:      u,
		bearer:       opts.BearerToken,
		userID:       opts.UserID,
		httpClient:   httpC,
		refreshToken: opts.RefreshToken,
		authURL:      opts.AuthURL,
		onRefresh:    opts.OnTokenRefresh,
	}, nil
}

// BaseURL returns the configured base URL as a string. Useful for
// logging and for the Socket.IO subscriber which dials the same host.
func (c *Client) BaseURL() string {
	return c.baseURL.String()
}

// BearerToken returns the current JWT (or empty if dev mode). Exposed so
// the Socket.IO subscriber can mirror REST auth without storing creds
// twice. Reflects the latest value after any in-flight refresh.
func (c *Client) BearerToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bearer
}

// do is the shared HTTP transport. method ∈ {GET, POST, PUT, DELETE}.
// path starts with /api/... ; query is appended if non-empty. body is
// marshaled as JSON if non-nil. response is JSON-decoded into out if
// out non-nil.
//
// Non-2xx replies are decoded into APIError and returned as an
// error ; the caller can errors.As to inspect StatusCode.
func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	err := c.doOnce(ctx, method, path, query, body, out)
	// Transparent recovery : a 401 means the access token expired ; trade
	// the refresh token for a fresh one and replay the request once. body
	// is re-marshaled on the retry (it's a value, not a consumed reader).
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized && c.tryRefresh(ctx) {
		return c.doOnce(ctx, method, path, query, body, out)
	}
	return err
}

func (c *Client) doOnce(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + path
	if query != nil {
		u.RawQuery = query.Encode()
	}

	var reqBody io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("client: marshal body: %w", err)
		}
		reqBody = bytes.NewReader(buf)
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reqBody)
	if err != nil {
		return fmt.Errorf("client: new request: %w", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if tok := c.BearerToken(); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	if c.userID != "" {
		req.Header.Set("X-User-ID", c.userID)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("client: %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("client: read body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		// Best-effort decode ; fall back to "raw body" for non-JSON.
		if jerr := json.Unmarshal(respBody, apiErr); jerr != nil {
			apiErr.Message = strings.TrimSpace(string(respBody))
		}
		if apiErr.Code == "" && apiErr.Message == "" {
			apiErr.Message = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return apiErr
	}

	if out != nil && len(respBody) > 0 {
		if err := json.Unmarshal(respBody, out); err != nil {
			return fmt.Errorf("client: decode response: %w", err)
		}
	}
	return nil
}

// tryRefresh trades the stored refresh token for a fresh access token
// and rewrites the client's auth in place. Returns false (no retry) when
// there's nothing to refresh with or the refresh itself fails. A single
// in-flight refresh is enforced so concurrent 401s don't stampede the
// rotating refresh token.
func (c *Client) tryRefresh(ctx context.Context) bool {
	c.mu.Lock()
	if c.refreshToken == "" || c.refreshing {
		c.mu.Unlock()
		return false
	}
	c.refreshing = true
	rt, au := c.refreshToken, c.authURL
	c.mu.Unlock()

	fresh, err := RefreshAccessToken(ctx, OAuthConfig{Issuer: au}, rt)

	c.mu.Lock()
	c.refreshing = false
	if err != nil || fresh == nil || fresh.AccessToken == "" {
		c.mu.Unlock()
		return false
	}
	c.bearer = fresh.AccessToken
	if fresh.RefreshToken != "" {
		c.refreshToken = fresh.RefreshToken
	}
	cb := c.onRefresh
	c.mu.Unlock()

	if cb != nil {
		cb(fresh)
	}
	return true
}

// Ping hits /health and returns nil if the daemon is reachable + 200.
// Used by Discovery and by the reconnect loop (CLI-9).
func (c *Client) Ping(ctx context.Context) error {
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + "/health"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("client: health %d", resp.StatusCode)
	}
	return nil
}
