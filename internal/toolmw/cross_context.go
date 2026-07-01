package toolmw

import (
	"context"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// RecentCall is one entry of the per-session recent-output trail that
// cross_context maintains.
type RecentCall struct {
	ModuleID string
	ToolName string
	Output   string
	At       time.Time
}

type recentKey struct{}

// RecentContext returns the recent tool outputs cross_context attached to ctx
// for the current session, or nil. A module Invoke can read this to take
// neighbouring tool results into account.
func RecentContext(ctx context.Context) []RecentCall {
	v, _ := ctx.Value(recentKey{}).([]RecentCall)
	return v
}

// crossContext shares recent tool outputs within a session : it exposes the
// trail on ctx before each call and records the result after.
//
// Fix vs the reference, whose trail was one list shared across every session :
// the trail is keyed by session, so a module never sees another session's tool
// outputs.
type crossContext struct {
	maxEntries int
	summaryMax int
	include    map[string]struct{}
	exclude    map[string]struct{}

	mu       sync.Mutex
	sessions map[string][]RecentCall
}

func newCrossContext(cfg map[string]any, deps Deps) (Middleware, error) {
	c := &crossContext{
		maxEntries: cfgInt(cfg, "max_entries", 20),
		summaryMax: cfgInt(cfg, "summary_max_chars", 500),
		sessions:   map[string][]RecentCall{},
	}
	if c.maxEntries < 1 {
		c.maxEntries = 1
	}
	if inc := cfgStrSlice(cfg, "include_modules"); len(inc) > 0 {
		c.include = toSet(inc)
	}
	if exc := cfgStrSlice(cfg, "exclude_modules"); len(exc) > 0 {
		c.exclude = toSet(exc)
	}
	return c, nil
}

func (c *crossContext) Name() string { return "cross_context" }

func (c *crossContext) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	c.mu.Lock()
	trail := c.sessions[cc.SessionID]
	if len(trail) > 0 {
		snapshot := make([]RecentCall, len(trail))
		copy(snapshot, trail)
		ctx = context.WithValue(ctx, recentKey{}, snapshot)
	}
	c.mu.Unlock()

	res, err := next(ctx, cc)
	if err != nil || !res.Success || !c.track(cc.ModuleID) {
		return res, err
	}

	entry := RecentCall{
		ModuleID: cc.ModuleID, ToolName: cc.ToolName,
		Output: truncate(renderResult(res), c.summaryMax), At: time.Now(),
	}
	c.mu.Lock()
	trail = append(c.sessions[cc.SessionID], entry)
	if len(trail) > c.maxEntries {
		trail = trail[len(trail)-c.maxEntries:]
	}
	c.sessions[cc.SessionID] = trail
	c.mu.Unlock()
	return res, nil
}

func (c *crossContext) track(moduleID string) bool {
	if _, no := c.exclude[moduleID]; no {
		return false
	}
	if c.include != nil {
		_, yes := c.include[moduleID]
		return yes
	}
	return true
}

func toSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

func cfgStrSlice(cfg map[string]any, key string) []string {
	if cfg == nil {
		return nil
	}
	switch v := cfg[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
