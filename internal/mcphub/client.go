package mcphub

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type FeaturedEntry struct {
	ServerID    string            `json:"server_id"`
	DisplayName string            `json:"display_name"`
	Description string            `json:"description"`
	Icon        string            `json:"icon"`
	Category    string            `json:"category"`
	Transport   string            `json:"transport"`
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Runtime     string            `json:"runtime"`
	Package     string            `json:"package"`
	URL         string            `json:"url"`
	DefaultEnv  map[string]string `json:"default_env"`

	EnvMapping      map[string]string `json:"env_mapping"`
	KeyDescriptions map[string]string `json:"key_descriptions"`

	OAuthProvider            string   `json:"oauth_provider"`
	OAuthStyle               string   `json:"oauth_style"`
	OAuthEnvTokenVar         string   `json:"oauth_env_token_var"`
	OAuthScopes              []string `json:"oauth_scopes"`
	OAuthKeyfileEnv          string   `json:"oauth_keyfile_env"`
	OAuthCredentialsEnv      string   `json:"oauth_credentials_env"`
	OAuthCredentialsFilename string   `json:"oauth_credentials_filename"`

	BinaryName   string  `json:"binary_name"`
	SmitherySlug string  `json:"smithery_slug"`
	Timeout      float64 `json:"timeout"`

	VerifiedAt   *time.Time `json:"verified_at"`
	LastTestedOK bool       `json:"last_tested_ok"`

	PersonalKeys     []string          `json:"personal_keys"`
	DigitornProvided map[string]string `json:"digitorn_provided"`
	HostedURL        *string           `json:"hosted_url"`
}

// Verified reports whether the entry carries a verification stamp.
func (e FeaturedEntry) Verified() bool { return e.VerifiedAt != nil }

// Hosted reports whether Digitorn hosts this server (zero-setup, no BYOK).
func (e FeaturedEntry) Hosted() bool { return e.HostedURL != nil && *e.HostedURL != "" }

// RegistryServer mirrors the Hub's McpRegistryRow — one entry from the mirrored
// upstream registry (the open firehose, semantically searchable on the Hub).
type RegistryServer struct {
	ServerID      string   `json:"server_id"`
	Name          string   `json:"name"`
	Description   string   `json:"description"`
	Runtime       string   `json:"runtime"`
	Package       string   `json:"package"`
	Transport     string   `json:"transport"`
	HasOAuth      bool     `json:"has_oauth"`
	EnvVarNames   []string `json:"env_var_names"`
	Version       string   `json:"version"`
	RepositoryURL string   `json:"repository_url"`
	Status        string   `json:"status"`
}

// Client talks to one Hub. Zero value is unusable — use NewClient.
type Client struct {
	base string
	http *http.Client
	ttl  time.Duration

	mu           sync.Mutex
	featured     []FeaturedEntry
	featuredAt   time.Time
	featuredGood bool

	pieces     []PieceEntry
	piecesAt   time.Time
	piecesGood bool
}

// NewClient builds a Hub client. baseURL defaults to https://hub.digitorn.ai.
func NewClient(baseURL string, timeout time.Duration, verifySSL bool) *Client {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = "https://hub.digitorn.ai"
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: !verifySSL}, //nolint:gosec — operator-controlled
	}
	return &Client{
		base: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Transport: tr, Timeout: timeout},
		ttl:  5 * time.Minute,
	}
}

type featuredResponse struct {
	Entries []FeaturedEntry `json:"entries"`
	Count   int             `json:"count"`
}

// Featured returns the curated catalog (cached for the client TTL). A failed
// fetch returns the error AND the last good cache when one exists, so a hub
// blip degrades to stale data instead of an empty catalog.
func (c *Client) Featured(ctx context.Context) ([]FeaturedEntry, error) {
	c.mu.Lock()
	if c.featuredGood && time.Since(c.featuredAt) < c.ttl {
		out := c.featured
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	var resp featuredResponse
	if err := c.getJSON(ctx, "/api/v1/mcp/featured?limit=500", &resp); err != nil {
		c.mu.Lock()
		stale, good := c.featured, c.featuredGood
		c.mu.Unlock()
		if good {
			return stale, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.featured, c.featuredGood, c.featuredAt = resp.Entries, true, time.Now()
	c.mu.Unlock()
	return resp.Entries, nil
}

// FeaturedByID fetches one featured entry (served from the cached list when warm).
func (c *Client) FeaturedByID(ctx context.Context, id string) (FeaturedEntry, bool, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if list, err := c.Featured(ctx); err == nil {
		for _, e := range list {
			if strings.ToLower(e.ServerID) == id {
				return e, true, nil
			}
		}
		return FeaturedEntry{}, false, nil
	}
	// Cache miss + list error: try the detail endpoint directly.
	var e FeaturedEntry
	err := c.getJSON(ctx, "/api/v1/mcp/featured/"+url.PathEscape(id), &e)
	if err != nil {
		return FeaturedEntry{}, false, err
	}
	return e, e.ServerID != "", nil
}

type registryBrowseResponse struct {
	Servers    []RegistryServer `json:"servers"`
	Count      int              `json:"count"`
	NextCursor string           `json:"next_cursor"`
}

// RegistryBrowse proxies the Hub's semantic registry search (q optional). cursor
// is the Hub's opaque offset cursor; limit is capped Hub-side.
func (c *Client) RegistryBrowse(ctx context.Context, q, cursor string, limit int) ([]RegistryServer, string, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	vals := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	if s := strings.TrimSpace(q); s != "" {
		vals.Set("q", s)
	}
	if cursor != "" {
		vals.Set("offset", cursor)
	}
	var resp registryBrowseResponse
	if err := c.getJSON(ctx, "/api/v1/mcp/registry/browse?"+vals.Encode(), &resp); err != nil {
		return nil, "", err
	}
	return resp.Servers, resp.NextCursor, nil
}

// ── Pieces (Activepieces connectors) ──────────────────────────────────

// PieceEntry mirrors the Hub's PieceRow — one Activepieces connector in the
// catalog. Pieces are stored as McpFeaturedEntry rows with category="pieces".
type PieceEntry struct {
	ServerID    string   `json:"server_id"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	Icon        string   `json:"icon"`
	AuthType    string   `json:"auth_type"`
	Category    string   `json:"category"`
	PersonalKeys []string `json:"personal_keys"`
	HostedURL   *string  `json:"hosted_url"`
	Priority    int      `json:"featured_priority"`
}

type piecesListResponse struct {
	Pieces []PieceEntry `json:"pieces"`
	Count  int          `json:"count"`
}

// PiecesList returns all available pieces from the hub catalog (cached for
// the client TTL). A failed fetch degrades to stale data when available.
func (c *Client) PiecesList(ctx context.Context) ([]PieceEntry, error) {
	c.mu.Lock()
	if c.piecesGood && time.Since(c.piecesAt) < c.ttl {
		out := c.pieces
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()

	var resp piecesListResponse
	if err := c.getJSON(ctx, "/api/v1/pieces?limit=500", &resp); err != nil {
		c.mu.Lock()
		stale, good := c.pieces, c.piecesGood
		c.mu.Unlock()
		if good {
			return stale, nil
		}
		return nil, err
	}
	c.mu.Lock()
	c.pieces, c.piecesGood, c.piecesAt = resp.Pieces, true, time.Now()
	c.mu.Unlock()
	return resp.Pieces, nil
}

// PiecesGet fetches one piece by ID from the hub catalog.
type PieceSystemConfig struct {
	ServerID         string         `json:"server_id"`
	AuthType         string         `json:"auth_type"`
	DigitornProvided map[string]any `json:"digitorn_provided"`
	DefaultEnv       map[string]any `json:"default_env"`
	EnvMapping       map[string]any `json:"env_mapping"`
	PersonalKeys     []string       `json:"personal_keys"`
}

func (c *Client) PiecesSystemConfig(ctx context.Context, id string) (*PieceSystemConfig, error) {
	key := os.Getenv("DIGITORN_DAEMON_KEY")
	if key == "" {
		return nil, fmt.Errorf("mcphub: DIGITORN_DAEMON_KEY not set")
	}
	id = strings.ToLower(strings.TrimSpace(id))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.base+"/api/v1/pieces/"+url.PathEscape(id)+"/system", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Daemon-Key", key)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("mcphub: system config for %s returned HTTP %d", id, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var out PieceSystemConfig
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) PiecesGet(ctx context.Context, id string) (PieceEntry, bool, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if list, err := c.PiecesList(ctx); err == nil {
		for _, e := range list {
			if strings.ToLower(e.ServerID) == id {
				return e, true, nil
			}
		}
		return PieceEntry{}, false, nil
	}
	// Cache miss + list error: try the detail endpoint directly.
	var e PieceEntry
	err := c.getJSON(ctx, "/api/v1/pieces/"+url.PathEscape(id), &e)
	if err != nil {
		return PieceEntry{}, false, err
	}
	return e, e.ServerID != "", nil
}

// PiecesBundleURL returns the URL to download a piece's .js bundle from the hub.
// The hub redirects to a presigned S3 URL (1h TTL).
func (c *Client) PiecesBundleURL(id string) string {
	return c.base + "/api/v1/pieces/bundles/" + url.PathEscape(id) + ".js"
}

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("mcphub: %s returned HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}
