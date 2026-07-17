package client

import (
	"context"
	"fmt"
	"net/url"
)

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

func (c *Client) EnableApp(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	return c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/enable", nil, nil, nil)
}

func (c *Client) DisableApp(ctx context.Context, appID string) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	return c.do(ctx, "POST", "/api/apps/"+url.PathEscape(appID)+"/disable", nil, nil, nil)
}

func (c *Client) SetBYOK(ctx context.Context, appID string, enabled bool) error {
	if appID == "" {
		return fmt.Errorf("client: appID required")
	}
	body := map[string]bool{"enabled": enabled}
	return c.do(ctx, "PUT", "/api/apps/"+url.PathEscape(appID)+"/byok", nil, body, nil)
}

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
