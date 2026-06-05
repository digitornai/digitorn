package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

// structToMap converts a Go struct to a map[string]any using `koanf` tags as keys.
// Nested structs become nested maps. Used to seed koanf with defaults.
func structToMap(v any) (map[string]any, error) {
	if v == nil {
		return map[string]any{}, nil
	}
	rv := reflect.ValueOf(v)
	if rv.Kind() == reflect.Pointer {
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		// Fallback for non-struct: JSON round-trip
		raw, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		out := map[string]any{}
		_ = json.Unmarshal(raw, &out)
		return out, nil
	}

	out := make(map[string]any, rv.NumField())
	t := rv.Type()
	for i := 0; i < rv.NumField(); i++ {
		field := t.Field(i)
		if !field.IsExported() {
			continue
		}
		tag := field.Tag.Get("koanf")
		if tag == "" || tag == "-" {
			continue
		}
		key := strings.SplitN(tag, ",", 2)[0]
		fv := rv.Field(i)
		switch fv.Kind() {
		case reflect.Struct:
			nested, err := structToMap(fv.Interface())
			if err != nil {
				return nil, fmt.Errorf("field %s: %w", field.Name, err)
			}
			out[key] = nested
		default:
			out[key] = fv.Interface()
		}
	}
	return out, nil
}
