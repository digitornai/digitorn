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

const DefaultTimeout = 30 * time.Second

type Options struct {
	BaseURL string

	BearerToken string

	UserID string

	HTTPClient *http.Client

	RefreshToken string
	AuthURL      string

	OnTokenRefresh func(*Credentials)
}

type Client struct {
	baseURL    *url.URL
	userID     string
	httpClient *http.Client

	mu           sync.Mutex
	bearer       string
	refreshToken string
	authURL      string
	onRefresh    func(*Credentials)
	refreshing   bool
}

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

func (c *Client) BaseURL() string {
	return c.baseURL.String()
}

func (c *Client) BearerToken() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bearer
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	err := c.doOnce(ctx, method, path, query, body, out)
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
