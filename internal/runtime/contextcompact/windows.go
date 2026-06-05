package contextcompact

import "strings"

// DefaultContextWindow is the conservative fallback when a model's window is
// unknown. Kept small on purpose: under-estimating only compacts a little
// early, while over-estimating risks a real overflow (the emergency path is the
// last resort). Mirrors the tool-injection planner's default.
const DefaultContextWindow = 8000

// modelWindows maps a model-name PREFIX to its documented context window
// (input+output tokens). Longest-prefix wins. These are public, vendor-stated
// values — not guesses; an explicit runtime.context.max_tokens always overrides
// this lookup, which only runs for max_tokens:0 (auto-detect).
var modelWindows = []struct {
	prefix string
	window int
}{
	{"gpt-4o", 128000},
	{"gpt-4.1", 1000000},
	{"gpt-4-turbo", 128000},
	{"gpt-4", 128000},
	{"gpt-5", 256000},
	{"o1", 128000},
	{"o3", 200000},
	{"o4", 200000},
	{"claude-3-5", 200000},
	{"claude-3-7", 200000},
	{"claude-sonnet-4", 200000},
	{"claude-opus-4", 200000},
	{"claude-3", 200000},
	{"claude", 200000},
	{"deepseek", 65536},
	{"gemini-1.5", 1000000},
	{"gemini-2", 1000000},
	{"gemini", 1000000},
	{"mistral-large", 128000},
	{"llama-3", 128000},
	{"qwen", 131072},
}

// ContextWindowFor resolves a model's context window for the auto-detect path
// (runtime.context.max_tokens:0). Returns DefaultContextWindow when the model is
// unknown. Matching is case-insensitive, longest-prefix-first on the model name;
// provider is accepted for future provider-specific overrides.
func ContextWindowFor(provider, model string) int {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "" {
		return DefaultContextWindow
	}
	best, bestLen := DefaultContextWindow, -1
	for _, e := range modelWindows {
		if strings.HasPrefix(m, e.prefix) && len(e.prefix) > bestLen {
			best, bestLen = e.window, len(e.prefix)
		}
	}
	return best
}
