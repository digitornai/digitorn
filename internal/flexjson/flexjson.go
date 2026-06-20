package flexjson

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

type Int int

func (f *Int) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		*f = 0
		return nil
	}
	switch {
	case v < 0:
		*f = 0
	case v > math.MaxInt32:
		*f = Int(math.MaxInt32)
	default:
		*f = Int(int(v))
	}
	return nil
}

type Bool bool

func (f *Bool) UnmarshalJSON(b []byte) error {
	s := strings.ToLower(strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`)))
	switch s {
	case "true", "1", "yes", "on":
		*f = true
	default:
		*f = false
	}
	return nil
}

type Content string

func (f *Content) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*f = ""
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*f = Content(s)
		return nil
	}
	var arr []any
	if err := json.Unmarshal(b, &arr); err == nil {
		lines := make([]string, 0, len(arr))
		for _, el := range arr {
			switch v := el.(type) {
			case string:
				lines = append(lines, v)
			case map[string]any:
				found := false
				for _, k := range arrayObjectKeys {
					if sv, ok := v[k].(string); ok {
						lines = append(lines, sv)
						found = true
						break
					}
				}
				if !found {
					b2, _ := json.Marshal(v)
					lines = append(lines, string(b2))
				}
			default:
				b2, _ := json.Marshal(el)
				lines = append(lines, string(b2))
			}
		}
		*f = Content(strings.Join(lines, "\n"))
		return nil
	}
	raw := strings.TrimSpace(string(b))
	if strings.HasPrefix(raw, "{") {
		inner := strings.ToLower(raw)
		if strings.Contains(inner, "--") ||
			strings.Contains(inner, "@import") ||
			strings.Contains(inner, "@keyframes") ||
			strings.Contains(inner, "@theme") ||
			strings.Contains(inner, "@layer") ||
			strings.Contains(inner, "@media") {
			*f = Content(raw)
			return nil
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(b, &obj); err == nil {
		for _, k := range objectStringKeys {
			if sv, ok := obj[k].(string); ok {
				*f = Content(sv)
				return nil
			}
		}
		b2, _ := json.MarshalIndent(obj, "", "  ")
		*f = Content(string(b2))
		return nil
	}
	*f = Content(strings.Trim(string(b), `"`))
	return nil
}

type Float64 float64

func (f *Float64) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(strings.Trim(strings.TrimSpace(string(b)), `"`))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) {
		*f = 0
		return nil
	}
	*f = Float64(v)
	return nil
}

var (
	arrayObjectKeys  = []string{"text", "content", "line", "value", "code", "source", "snippet"}
	objectStringKeys = []string{"content", "text", "body", "code", "source"}
)
