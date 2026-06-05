package catalog

import "github.com/mbathepaul/digitorn/internal/compiler/schema"

func defaultProviders() []string {
	extra := []string{"openrouter", "minimax", "featherless", "claude_code"}
	out := make([]string, 0, len(schema.KnownProviders)+len(extra))
	for _, p := range schema.KnownProviders {
		out = append(out, string(p))
	}
	return append(out, extra...)
}
