package toolmw

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

type autoHeal struct {
	maxSuggestions  int
	includeCrossMod bool
	resolve         ToolResolver
	heals           atomic.Uint64
}

func newAutoHeal(cfg map[string]any, deps Deps) (Middleware, error) {
	return &autoHeal{
		maxSuggestions:  cfgInt(cfg, "max_suggestions", 3),
		includeCrossMod: cfgBool(cfg, "include_cross_server", true),
		resolve:         deps.ToolResolver,
	}, nil
}

func (a *autoHeal) Name() string { return "auto_heal" }

func (a *autoHeal) Handle(ctx context.Context, cc CallContext, next Next) (tool.Result, error) {
	res, err := next(ctx, cc)
	if err != nil || res.Success || a.resolve == nil {
		return res, err
	}

	all := a.resolve(cc.ModuleID, cc.ToolName)
	if len(all) == 0 {
		return res, nil
	}

	same := make([]ToolSuggestion, 0, a.maxSuggestions)
	cross := make([]ToolSuggestion, 0, a.maxSuggestions)
	for _, s := range all {
		if s.ModuleID == cc.ModuleID {
			same = append(same, s)
		} else if a.includeCrossMod {
			cross = append(cross, s)
		}
	}
	picked := same
	if len(picked) > a.maxSuggestions {
		picked = picked[:a.maxSuggestions]
	}
	for _, s := range cross {
		if len(picked) >= a.maxSuggestions {
			break
		}
		picked = append(picked, s)
	}
	if len(picked) == 0 {
		return res, nil
	}

	a.heals.Add(1)
	var b strings.Builder
	b.WriteString("\n\nSuggested alternatives:")
	for _, s := range picked {
		b.WriteString("\n  - ")
		if s.ModuleID != cc.ModuleID {
			b.WriteString("[" + s.ModuleID + "] ")
		}
		b.WriteString(s.ToolName)
		if s.Description != "" {
			b.WriteString(": " + s.Description)
		}
	}
	res.Error += b.String()
	return res, nil
}
