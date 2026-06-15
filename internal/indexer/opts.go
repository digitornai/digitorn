package indexer

import "errors"

var errInvalidURL = errors.New("indexer/web: invalid http(s) url")
var errNoPath = errors.New("indexer/file: path required")

func optString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func optInt(m map[string]any, k string) (int, bool) {
	switch v := m[k].(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	}
	return 0, false
}

func optBool(m map[string]any, k string) (bool, bool) {
	if v, ok := m[k].(bool); ok {
		return v, true
	}
	return false, false
}

func optStrings(m map[string]any, k string) []string {
	switch v := m[k].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
