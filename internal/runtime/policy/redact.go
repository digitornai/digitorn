package policy

import "strings"

// SensitiveKeyPatterns is the documented allow-list of substring
// patterns that mark a tool_param key as "must be redacted in audit
// rows". Case-insensitive substring match.
//
// The list mirrors the conservative defaults used by similar
// security tooling (CloudWatch param redaction, Datadog APM, etc.).
// It is package-level (not a flag) so the runtime path stays
// allocation-free on the hot path ; callers who need different
// patterns (e.g. an app with a custom sensitive field) can pass a
// custom set into RedactParams.
var SensitiveKeyPatterns = []string{
	"password",
	"passwd",
	"secret",
	"api_key",
	"apikey",
	"api-key",
	"token",
	"auth",
	"authorization",
	"credentials",
	"credential",
	"private_key",
	"privatekey",
	"bearer",
	"x-api-key",
	"ssh",
	"refresh_token",
	"access_token",
	"client_secret",
	"session_id", // can be sensitive in some apps
	"jwt",
}

// RedactedPlaceholder is the canonical replacement value for
// redacted params. Constant so consumers can grep for it and so
// the JSON shape stays stable across releases.
const RedactedPlaceholder = "[REDACTED]"

// RedactParams returns a copy of `params` with sensitive keys
// replaced by RedactedPlaceholder. Pure : the original map is NOT
// mutated. Used by SG-6 to populate
// SecurityDecisionPayload.ParamsRedacted.
//
// patterns is the list of substring patterns to match against each
// key (case-insensitive). When nil, the package-level
// SensitiveKeyPatterns is used.
//
// Nested maps AND slices are recursed into so a sensitive key at any
// depth — {auth: {token: "..."}} or {creds: [{api_key: "..."}]} — is
// redacted. Scalar values are kept as-is.
func RedactParams(params map[string]any, patterns []string) map[string]any {
	if len(params) == 0 {
		return params
	}
	if patterns == nil {
		patterns = SensitiveKeyPatterns
	}
	out := make(map[string]any, len(params))
	for k, v := range params {
		if isSensitive(k, patterns) {
			out[k] = RedactedPlaceholder
			continue
		}
		out[k] = redactValue(v, patterns)
	}
	return out
}

// redactValue recurses into nested maps AND slices so a sensitive key buried at
// any depth is still redacted — including inside arrays, e.g.
// {creds: [{api_key: "..."}]} or {headers: [{authorization: "..."}]}. The
// previous version recursed into maps only, so a secret one slice deep leaked
// verbatim into the audit row. Scalars are returned unchanged : key-based
// redaction can only act on a keyed value, so a bare string element in an array
// (no key to match) is left as-is.
func redactValue(v any, patterns []string) any {
	switch t := v.(type) {
	case map[string]any:
		return RedactParams(t, patterns)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e, patterns)
		}
		return out
	default:
		return v
	}
}

// isSensitive reports whether the given key matches any of the
// sensitive patterns (case-insensitive substring).
func isSensitive(key string, patterns []string) bool {
	lower := strings.ToLower(key)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
