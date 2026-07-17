package policy

import "strings"

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
	"session_id",
	"jwt",
}

const RedactedPlaceholder = "[REDACTED]"

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

func isSensitive(key string, patterns []string) bool {
	lower := strings.ToLower(key)
	for _, p := range patterns {
		if strings.Contains(lower, strings.ToLower(p)) {
			return true
		}
	}
	return false
}
