package client

import (
	"context"
	"fmt"
	"net/url"
)

// ListSecrets returns all secrets for an app.
// Endpoint : GET /api/apps/{app_id}/secrets
func (c *Client) ListSecrets(ctx context.Context, appID string) (map[string]string, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var out struct {
		Secrets map[string]string `json:"secrets"`
		Count   int               `json:"count"`
	}
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/secrets", nil, nil, &out); err != nil {
		return nil, err
	}
	return out.Secrets, nil
}

// GetSecret returns a single secret value.
// Endpoint : GET /api/apps/{app_id}/secrets/{key}
func (c *Client) GetSecret(ctx context.Context, appID, key string) (string, error) {
	if appID == "" || key == "" {
		return "", fmt.Errorf("client: appID and key required")
	}
	var out struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(key), nil, nil, &out); err != nil {
		return "", err
	}
	return out.Value, nil
}

// SetSecret sets a single secret value.
// Endpoint : PUT /api/apps/{app_id}/secrets/{key}
func (c *Client) SetSecret(ctx context.Context, appID, key, value string) error {
	if appID == "" || key == "" {
		return fmt.Errorf("client: appID and key required")
	}
	body := map[string]string{"value": value}
	return c.do(ctx, "PUT", "/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(key), nil, body, nil)
}

// DeleteSecret removes a single secret.
// Endpoint : DELETE /api/apps/{app_id}/secrets/{key}
func (c *Client) DeleteSecret(ctx context.Context, appID, key string) error {
	if appID == "" || key == "" {
		return fmt.Errorf("client: appID and key required")
	}
	return c.do(ctx, "DELETE", "/api/apps/"+url.PathEscape(appID)+"/secrets/"+url.PathEscape(key), nil, nil, nil)
}

// AppStatus returns the health check status for an app.
// Endpoint : GET /api/apps/{app_id}/status
func (c *Client) AppStatus(ctx context.Context, appID string) (map[string]any, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var out map[string]any
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/status", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}



// DaemonStats returns daemon-level statistics.
// Endpoint : GET /api/daemon/stats
func (c *Client) DaemonStats(ctx context.Context) (map[string]any, error) {
	var out map[string]any
	if err := c.do(ctx, "GET", "/api/daemon/stats", nil, nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}
