package client

import (
	"context"
	"fmt"
	"net/url"
)

// AppModel returns the model the app's entry agent is configured to use,
// pulled from the compiled manifest (GET /api/apps/{id}/manifest). Empty
// when no agent declares a model (the brain inherits a provider default).
//
// The manifest serialises the daemon's schema.AppDefinition, whose Go
// structs carry only yaml tags — so JSON keys are the capitalised field
// names ("Agents", "Brain", "Model"). We navigate defensively, tolerating
// either casing, rather than mirroring the whole schema here.
func (c *Client) AppModel(ctx context.Context, appID string) (string, error) {
	if appID == "" {
		return "", fmt.Errorf("client: appID required")
	}
	var raw map[string]any
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/manifest", nil, nil, &raw); err != nil {
		return "", err
	}
	return manifestModel(raw), nil
}

func manifestModel(raw map[string]any) string {
	agents, ok := firstSlice(raw, "agents", "Agents")
	if !ok {
		return ""
	}
	for _, a := range agents {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		brain, ok := firstMap(am, "brain", "Brain")
		if !ok {
			continue
		}
		if m := firstString(brain, "model", "Model"); m != "" {
			return m
		}
	}
	return ""
}

func firstSlice(m map[string]any, keys ...string) ([]any, bool) {
	for _, k := range keys {
		if v, ok := m[k].([]any); ok {
			return v, true
		}
	}
	return nil, false
}

func firstMap(m map[string]any, keys ...string) (map[string]any, bool) {
	for _, k := range keys {
		if v, ok := m[k].(map[string]any); ok {
			return v, true
		}
	}
	return nil, false
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
