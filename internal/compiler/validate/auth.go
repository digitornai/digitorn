package validate

import (
	"fmt"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/schema"
)

func (v *validator) checkBrainAuth() {
	for i, a := range v.def.Agents {
		v.brainAuth(a.Brain, fmt.Sprintf("agents.%d.brain", i))
	}
}

func (v *validator) brainAuth(b schema.Brain, base string) {
	if b.Provider != "" && !isLocalProvider(b.Provider) && !hasAuth(b) {
		v.errf(diagnostic.CodeBrainNoAuth, base,
			"provider %q requires credential, provider_id, or config.api_key", b.Provider)
	}
	if b.Fallback != nil {
		v.brainAuth(*b.Fallback, base+".fallback")
	}
}

func (v *validator) checkFallback() {
	for i, a := range v.def.Agents {
		if a.Brain.Fallback == nil {
			continue
		}
		f := a.Brain.Fallback
		if f.Provider == a.Brain.Provider && f.Model == a.Brain.Model {
			v.errf(diagnostic.CodeFallbackSameAsPrimary,
				fmt.Sprintf("agents.%d.brain.fallback", i),
				"fallback is identical to the primary brain (%s/%s); use a different provider or model",
				f.Provider, f.Model)
		}
	}
}

func isLocalProvider(p string) bool {
	if _, ok := schema.LocalProviders[schema.Provider(p)]; ok {
		return true
	}
	return false
}

func hasAuth(b schema.Brain) bool {
	if b.ProviderID != "" {
		return true
	}
	if c, ok := b.Credential.(string); ok && c != "" {
		return true
	}
	if c, ok := b.Credential.(map[string]any); ok && len(c) > 0 {
		return true
	}
	if v, ok := b.Config["api_key"]; ok {
		if s, isStr := v.(string); isStr && s != "" {
			return true
		}
	}
	if v, ok := b.Config["base_url"]; ok {
		if s, isStr := v.(string); isStr && (containsLocalhost(s) || s == "claude-code") {
			return true
		}
	}
	return false
}

func containsLocalhost(s string) bool {
	for _, sub := range []string{"localhost", "127.0.0.1", "::1"} {
		if indexSubstring(s, sub) >= 0 {
			return true
		}
	}
	return false
}

func indexSubstring(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
