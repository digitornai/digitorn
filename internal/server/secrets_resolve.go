package server

import "regexp"

// secretPlaceholderRe matches {{secret.NAME}} in any app config string.
var secretPlaceholderRe = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z0-9_]+)\s*\}\}`)

type secretLookup func(key string) (string, bool)

// resolveSecretPlaceholders deep-copies v, replacing every {{secret.X}} with
// lookup(X). Unresolved placeholders are left intact.
func resolveSecretPlaceholders(v any, lookup secretLookup) any {
	if lookup == nil {
		return v
	}
	switch t := v.(type) {
	case string:
		return secretPlaceholderRe.ReplaceAllStringFunc(t, func(match string) string {
			m := secretPlaceholderRe.FindStringSubmatch(match)
			if val, ok := lookup(m[1]); ok && val != "" {
				return val
			}
			return match
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = resolveSecretPlaceholders(vv, lookup)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = resolveSecretPlaceholders(vv, lookup)
		}
		return out
	}
	return v
}
