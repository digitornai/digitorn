package channels

import (
	"regexp"
	"strings"
)

// Sanitization bounds (security.py:84 sanitize_payload).
const (
	maxSanitizeDepth = 10
	maxStringLen     = 10000
	maxDictKeys      = 200
	maxListItems     = 500
)

// dangerousKeys are stripped from any incoming payload (prototype-pollution /
// Python-introspection vectors). security.py:84.
var dangerousKeys = map[string]struct{}{
	"__proto__": {}, "__class__": {}, "__import__": {}, "constructor": {},
	"__globals__": {}, "__builtins__": {}, "__subclasses__": {},
}

func isDangerousKey(k string) bool {
	if _, bad := dangerousKeys[k]; bad {
		return true
	}
	return strings.HasPrefix(k, "__") || strings.Contains(k, "$$")
}

// SanitizePayload defensively normalizes an untrusted inbound payload: strips
// dangerous keys, caps nesting depth, string length, dict size and list size.
// Faithful to the Python sanitize_payload guards (security measures 4–8).
func SanitizePayload(p map[string]any) map[string]any {
	if p == nil {
		return map[string]any{}
	}
	out, _ := sanitizeValue(p, 0).(map[string]any)
	if out == nil {
		return map[string]any{}
	}
	return out
}

func sanitizeValue(v any, depth int) any {
	if depth > maxSanitizeDepth {
		return nil
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any)
		n := 0
		for k, val := range x {
			if n >= maxDictKeys {
				break
			}
			if isDangerousKey(k) {
				continue
			}
			out[k] = sanitizeValue(val, depth+1)
			n++
		}
		return out
	case map[any]any:
		out := make(map[string]any)
		n := 0
		for k, val := range x {
			if n >= maxDictKeys {
				break
			}
			ks, ok := k.(string)
			if !ok || isDangerousKey(ks) {
				continue
			}
			out[ks] = sanitizeValue(val, depth+1)
			n++
		}
		return out
	case []any:
		out := make([]any, 0, len(x))
		for i, val := range x {
			if i >= maxListItems {
				break
			}
			out = append(out, sanitizeValue(val, depth+1))
		}
		return out
	case string:
		if len(x) > maxStringLen {
			return x[:maxStringLen]
		}
		return x
	default:
		return v
	}
}

// secretPatterns are the outbound secret signatures masked before a reply leaves
// the service (security.py:162 filter_secrets — 8 families). Order does not
// matter; each is replaced independently with the redaction marker.
var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`sk-ant-[A-Za-z0-9_\-]{20,}`),                           // Anthropic
	regexp.MustCompile(`sk-[A-Za-z0-9_\-]{20,}`),                               // OpenAI
	regexp.MustCompile(`xox[baprs]-[A-Za-z0-9\-]{10,}`),                        // Slack
	regexp.MustCompile(`ghp_[A-Za-z0-9]{20,}`),                                 // GitHub PAT
	regexp.MustCompile(`glpat-[A-Za-z0-9_\-]{16,}`),                            // GitLab PAT
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),                                     // AWS access key
	regexp.MustCompile(`eyJ[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+\.[A-Za-z0-9_\-]+`), // JWT
	regexp.MustCompile(`(?i)\b(?:Bearer|Basic)\s+[A-Za-z0-9._\-+/=]{8,}`),      // Authorization tokens
	regexp.MustCompile(`dk_[A-Za-z0-9]{16,}`),                                  // Digitorn key
}

const redaction = "***"

// FilterSecrets masks known secret signatures in an outbound string. Applied to
// agent replies before they are sent on a channel (security measure 10).
func FilterSecrets(s string) string {
	if s == "" {
		return s
	}
	for _, re := range secretPatterns {
		s = re.ReplaceAllString(s, redaction)
	}
	return s
}

// FilterSecretsIn walks a nested structure masking secrets in every string
// (security measure 11, filter_secrets_in_dict).
func FilterSecretsIn(v any) any {
	switch x := v.(type) {
	case string:
		return FilterSecrets(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = FilterSecretsIn(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = FilterSecretsIn(e)
		}
		return out
	default:
		return v
	}
}
