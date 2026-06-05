package client

import (
	"context"
	"fmt"
	"net/url"
)

// ListApps returns every installed app the daemon knows about. By
// default the daemon hides disabled apps ; pass includeDisabled=true
// to surface them too (used by the `digitorn list --all` flag).
//
// Endpoint : GET /api/apps?include_disabled=...
func (c *Client) ListApps(ctx context.Context, includeDisabled bool) ([]App, error) {
	q := url.Values{}
	if includeDisabled {
		q.Set("include_disabled", "true")
	}
	var out ListAppsResponse
	if err := c.do(ctx, "GET", "/api/apps", q, nil, &out); err != nil {
		return nil, err
	}
	return out.Apps, nil
}

// GetApp returns a single app's metadata. Returns *APIError with
// StatusCode 404 when the app is not installed.
//
// Endpoint : GET /api/apps/{app_id}
func (c *Client) GetApp(ctx context.Context, appID string) (*App, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var out App
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID), nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// InstallApp installs (or upgrades) an app from the given source URI :
//   - /abs/or/relative/path    — local filesystem
//   - hub://publisher/pkg@1.0  — digitorn hub
//   - builtin://name           — built-in example
//
// Returns the install response (matching the daemon's
// internal/server.installResponse shape).
//
// Endpoint : POST /api/apps/install
func (c *Client) InstallApp(ctx context.Context, source string) (*InstallResponse, error) {
	if source == "" {
		return nil, fmt.Errorf("client: source required")
	}
	body := map[string]string{"source": source}
	var out InstallResponse
	if err := c.do(ctx, "POST", "/api/apps/install", nil, body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UninstallApp removes the install dir + DB row. purge=true signals
// the daemon to drop session state too (V1 only records intent, real
// purge lands later — but we expose the flag now so the wire contract
// is stable).
//
// Endpoint : POST /api/apps/{app_id}/uninstall?purge=true|false
func (c *Client) UninstallApp(ctx context.Context, appID string, purge bool) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	q := url.Values{}
	if purge {
		q.Set("purge", "true")
	}
	return c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/uninstall", q, nil, nil)
}

// EnableApp flips enabled=true. Idempotent.
// Endpoint : POST /api/apps/{app_id}/enable
func (c *Client) EnableApp(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	return c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/enable", nil, nil, nil)
}

// DisableApp flips enabled=false. Idempotent.
// Endpoint : POST /api/apps/{app_id}/disable
func (c *Client) DisableApp(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	return c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/disable", nil, nil, nil)
}

// SetBYOK toggles the per-installation BYOK flag : true = dial the
// provider directly with the brain-declared credential ; false =
// route through the digitorn LLM gateway. Idempotent.
//
// Endpoint : PUT /api/apps/{app_id}/byok
func (c *Client) SetBYOK(ctx context.Context, appID string, enabled bool) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	body := map[string]bool{"enabled": enabled}
	return c.do(ctx, "PUT", "/api/apps/"+url.PathEscape(appID)+"/byok", nil, body, nil)
}

// ReloadApp recompiles the app from its on-disk source. Used after
// the operator edits app.yaml by hand.
//
// Endpoint : POST /api/apps/{app_id}/reload
func (c *Client) ReloadApp(ctx context.Context, appID string) (*App, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var out App
	if err := c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/reload", nil, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
