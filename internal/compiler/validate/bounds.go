package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func (v *validator) checkBounds() {
	v.checkApp()
	v.checkRuntimeBounds()
	for i, a := range v.def.Agents {
		v.checkBrainBounds(a.Brain, fmt.Sprintf("agents.%d.brain", i))
		if a.Pool != nil {
			v.checkPool(*a.Pool, fmt.Sprintf("agents.%d.pool", i))
		}
	}
	if v.def.Tools != nil && v.def.Tools.Capabilities != nil {
		c := v.def.Tools.Capabilities
		if c.ApprovalTimeout != 0 && (c.ApprovalTimeout < 30 || c.ApprovalTimeout > 3600) {
			v.errf(diagnostic.CodeOutOfRange, "tools.capabilities.approval_timeout",
				"approval_timeout must be in [30, 3600] (got %d)", c.ApprovalTimeout)
		}
	}
	if v.def.Security != nil && v.def.Security.Sandbox != nil {
		v.checkSandbox(*v.def.Security.Sandbox, "security.sandbox")
	}
	if v.def.UI != nil {
		v.checkUI(*v.def.UI, "ui")
	}
	if v.def.Runtime != nil {
		for i, t := range v.def.Runtime.Triggers {
			if t.Port != 0 && (t.Port < 1024 || t.Port > 65535) {
				v.errf(diagnostic.CodeOutOfRange,
					fmt.Sprintf("runtime.triggers.%d.port", i),
					"trigger port must be in [1024, 65535] (got %d)", t.Port)
			}
		}
		if v.def.Runtime.Context != nil {
			v.checkContext(*v.def.Runtime.Context, "runtime.context")
		}
	}
}

func (v *validator) checkApp() {
	a := v.def.App
	if l := len(a.ShortName); l > 32 {
		v.errf(diagnostic.CodeBadLength, "app.short_name",
			"short_name must be at most 32 chars (got %d)", l)
	}
}

func (v *validator) checkRuntimeBounds() {
	rt := v.def.Runtime
	if rt == nil {
		return
	}
	if rt.MaxTurns != 0 && rt.MaxTurns < 1 {
		v.errf(diagnostic.CodeOutOfRange, "runtime.max_turns",
			"max_turns must be >= 1 (got %d)", rt.MaxTurns)
	}
	if rt.Timeout != 0 && rt.Timeout <= 0 {
		v.errf(diagnostic.CodeOutOfRange, "runtime.timeout",
			"timeout must be > 0 (got %g)", rt.Timeout)
	}
	if rt.MaxSessionsPerUser < 0 {
		v.errf(diagnostic.CodeOutOfRange, "runtime.max_sessions_per_user",
			"max_sessions_per_user must be >= 0 (got %d)", rt.MaxSessionsPerUser)
	}
	if rt.MaxConcurrentActivations != 0 && rt.MaxConcurrentActivations < 1 {
		v.errf(diagnostic.CodeOutOfRange, "runtime.max_concurrent_activations",
			"max_concurrent_activations must be >= 1 (got %d)", rt.MaxConcurrentActivations)
	}
}

func (v *validator) checkBrainBounds(b schema.Brain, base string) {
	if b.Temperature != nil && (*b.Temperature < 0 || *b.Temperature > 2) {
		v.errf(diagnostic.CodeOutOfRange, base+".temperature",
			"temperature must be in [0, 2] (got %g)", *b.Temperature)
	}
	if b.TopP != nil && (*b.TopP < 0 || *b.TopP > 1) {
		v.errf(diagnostic.CodeOutOfRange, base+".top_p",
			"top_p must be in [0, 1] (got %g)", *b.TopP)
	}
	if b.MaxTokens != nil && *b.MaxTokens <= 0 {
		v.errf(diagnostic.CodeOutOfRange, base+".max_tokens",
			"max_tokens must be > 0 (got %d)", *b.MaxTokens)
	}
	if b.Timeout != nil && *b.Timeout <= 0 {
		v.errf(diagnostic.CodeOutOfRange, base+".timeout",
			"timeout must be > 0 (got %g)", *b.Timeout)
	}
	if b.MaxImagesPerTurn < 0 || b.MaxImagesPerTurn > 100 {
		v.errf(diagnostic.CodeOutOfRange, base+".max_images_per_turn",
			"max_images_per_turn must be in [0, 100] (got %d)", b.MaxImagesPerTurn)
	}
	if b.Fallback != nil {
		v.checkBrainBounds(*b.Fallback, base+".fallback")
	}
}

func (v *validator) checkPool(p schema.AgentPoolConfig, base string) {
	if p.MaxWorkers != 0 && (p.MaxWorkers < 1 || p.MaxWorkers > 100) {
		v.errf(diagnostic.CodeOutOfRange, base+".max_workers",
			"max_workers must be in [1, 100] (got %d)", p.MaxWorkers)
	}
	if p.AutoRetry < 0 || p.AutoRetry > 5 {
		v.errf(diagnostic.CodeOutOfRange, base+".auto_retry",
			"auto_retry must be in [0, 5] (got %d)", p.AutoRetry)
	}
}

func (v *validator) checkSandbox(s schema.SandboxConfig, base string) {
	if s.PoolSize != 0 && (s.PoolSize < 1 || s.PoolSize > 32) {
		v.errf(diagnostic.CodeOutOfRange, base+".pool_size",
			"pool_size must be in [1, 32] (got %d)", s.PoolSize)
	}
	if s.PoolMax != 0 && (s.PoolMax < 1 || s.PoolMax > 64) {
		v.errf(diagnostic.CodeOutOfRange, base+".pool_max",
			"pool_max must be in [1, 64] (got %d)", s.PoolMax)
	}
	if s.SessionTimeout != 0 && s.SessionTimeout < 60 {
		v.errf(diagnostic.CodeOutOfRange, base+".session_timeout",
			"session_timeout must be >= 60 (got %d)", s.SessionTimeout)
	}
	if s.IdleTimeout != 0 && s.IdleTimeout < 30 {
		v.errf(diagnostic.CodeOutOfRange, base+".idle_timeout",
			"idle_timeout must be >= 30 (got %d)", s.IdleTimeout)
	}
}

func (v *validator) checkUI(u schema.UIBlock, base string) {
	if u.Workspace != nil {
		if w := u.Workspace.WidthPct; w != 0 && (w < 10 || w > 90) {
			v.errf(diagnostic.CodeOutOfRange, base+".workspace.width_pct",
				"width_pct must be in [10, 90] (got %d)", w)
		}
	}
	if u.Activity != nil {
		if n := u.Activity.MaxRecent; n != 0 && (n < 5 || n > 500) {
			v.errf(diagnostic.CodeOutOfRange, base+".activity.max_recent",
				"max_recent must be in [5, 500] (got %d)", n)
		}
	}
}

func (v *validator) checkContext(c schema.ContextConfig, base string) {
	if c.MaxTokens < 0 || c.MaxTokens > 2_000_000 {
		v.errf(diagnostic.CodeOutOfRange, base+".max_tokens",
			"max_tokens must be in [0, 2_000_000] (got %d)", c.MaxTokens)
	}
	if c.CompressionTrigger != 0 && (c.CompressionTrigger < 0 || c.CompressionTrigger > 1) {
		v.errf(diagnostic.CodeOutOfRange, base+".compression_trigger",
			"compression_trigger must be in [0, 1] (got %g)", c.CompressionTrigger)
	}
}

// Keep schema imported above; refer to it explicitly to satisfy the linter.
var _ = schema.ModeBackground
