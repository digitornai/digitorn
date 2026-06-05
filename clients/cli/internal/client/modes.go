package client

import (
	"context"
	"fmt"
	"net/url"
)

// AppModes returns the composer modes an app declares (runtime.modes), in YAML
// declaration order — the order the CLI cycles through. Empty when the app
// declares none. Pulled from the compiled manifest, same as AppModel ; the
// schema carries only yaml tags so writeJSON emits capitalised Go field names
// ("Runtime", "Modes", "ModesOrder"), which we navigate tolerantly.
func (c *Client) AppModes(ctx context.Context, appID string) ([]Mode, error) {
	if appID == "" {
		return nil, fmt.Errorf("client: appID required")
	}
	var raw map[string]any
	if err := c.do(ctx, "GET", "/api/apps/"+url.PathEscape(appID)+"/manifest", nil, nil, &raw); err != nil {
		return nil, err
	}
	return manifestModes(raw), nil
}

func manifestModes(raw map[string]any) []Mode {
	rt, ok := firstMap(raw, "runtime", "Runtime")
	if !ok {
		return nil
	}
	defs, ok := firstMap(rt, "modes", "Modes")
	if !ok || len(defs) == 0 {
		return nil
	}
	// Prefer the captured YAML order ; fall back to map iteration when absent.
	var ids []string
	if order, ok := firstSlice(rt, "modes_order", "ModesOrder"); ok {
		for _, v := range order {
			if s, ok := v.(string); ok {
				ids = append(ids, s)
			}
		}
	}
	if len(ids) == 0 {
		for id := range defs {
			ids = append(ids, id)
		}
	}
	out := make([]Mode, 0, len(ids))
	for _, id := range ids {
		md, ok := defs[id].(map[string]any)
		if !ok {
			continue
		}
		label := firstString(md, "label", "Label")
		if label == "" {
			label = id
		}
		out = append(out, Mode{
			ID:          id,
			Label:       label,
			Description: firstString(md, "description", "Description"),
			Icon:        firstString(md, "icon", "Icon"),
		})
	}
	return out
}

// SessionActiveMode returns the session's sticky mode id (its last-used mode),
// so the CLI can restore the switcher to it on reload instead of resetting to
// the app default. Empty when the session has no stored mode yet.
func (c *Client) SessionActiveMode(ctx context.Context, appID, sessionID string) (string, error) {
	if appID == "" || sessionID == "" {
		return "", fmt.Errorf("client: appID and sessionID required")
	}
	var raw map[string]any
	path := "/api/apps/" + url.PathEscape(appID) + "/sessions/" + url.PathEscape(sessionID)
	if err := c.do(ctx, "GET", path, nil, nil, &raw); err != nil {
		return "", err
	}
	return firstString(raw, "active_mode", "ActiveMode"), nil
}
