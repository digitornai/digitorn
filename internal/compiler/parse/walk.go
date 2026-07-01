package parse

import (
	"fmt"
	"reflect"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

var yamlUnmarshalerType = reflect.TypeOf((*yaml.Unmarshaler)(nil)).Elem()

// permissiveStructs accept extra keys without emitting DGT-E0101. Each entry
// matches the struct's short name "package.TypeName".
var permissiveStructs = map[string]struct{}{
	"schema.ModuleBlock":   {},
	"schema.HookCondition": {},
	"schema.HookAction":    {},
}

// CheckUnknownFields walks node and target in parallel and emits DGT-E0101
// for any mapping key without a matching yaml-tagged struct field.
func CheckUnknownFields(file string, node *yaml.Node, target any, path string, bag *diagnostic.Bag) {
	if node == nil || target == nil {
		return
	}
	rv := reflect.ValueOf(target)
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return
		}
		rv = rv.Elem()
	}
	walk(file, node, rv, rv.Type(), path, bag)
}

func walk(file string, node *yaml.Node, rv reflect.Value, rt reflect.Type, path string, bag *diagnostic.Bag) {
	for rv.Kind() == reflect.Pointer || rv.Kind() == reflect.Interface {
		if rv.IsNil() {
			break
		}
		rv = rv.Elem()
		rt = rv.Type()
	}
	if implementsUnmarshaler(rt) {
		return
	}

	switch rt.Kind() {
	case reflect.Struct:
		walkStruct(file, node, rv, rt, path, bag)
	case reflect.Slice, reflect.Array:
		if !IsSequence(node) {
			return
		}
		elem := rt.Elem()
		for i, item := range node.Content {
			child := fmt.Sprintf("%s[%d]", path, i)
			if rv.Len() > i {
				walk(file, item, rv.Index(i), elem, child, bag)
			} else {
				walk(file, item, reflect.New(elem).Elem(), elem, child, bag)
			}
		}
	case reflect.Map:
		if !IsMapping(node) || rt.Key().Kind() != reflect.String {
			return
		}
		val := rt.Elem()
		if val.Kind() == reflect.Interface {
			return
		}
		for i := 0; i < len(node.Content)-1; i += 2 {
			k, v := node.Content[i], node.Content[i+1]
			if k.Kind != yaml.ScalarNode {
				continue
			}
			walk(file, v, reflect.New(val).Elem(), val, joinKey(path, k.Value), bag)
		}
	}
}

func walkStruct(file string, node *yaml.Node, rv reflect.Value, rt reflect.Type, path string, bag *diagnostic.Bag) {
	if node.Kind != yaml.MappingNode {
		return
	}
	fields := buildFieldIndex(rt)
	permissive := isPermissive(rt)

	for i := 0; i < len(node.Content)-1; i += 2 {
		key, val := node.Content[i], node.Content[i+1]
		if key.Kind != yaml.ScalarNode {
			continue
		}
		field, ok := fields[key.Value]
		if !ok {
			if permissive {
				continue
			}
			diag := diagnostic.Errorf(diagnostic.CodeUnknownField, NodePos(file, key),
				"%sunknown field %q", pathPrefix(joinKey(path, "")), key.Value)
			pool := make([]string, 0, len(fields))
			for k := range fields {
				pool = append(pool, k)
			}
			if s, ok := closestMatch(key.Value, pool); ok {
				diag = diag.WithSuggestion(s, fmt.Sprintf("did you mean %q?", s))
			}
			bag.Add(diag)
			continue
		}
		walk(file, val, rv.FieldByIndex(field.Index), field.Type, joinKey(path, key.Value), bag)
	}
}

func isPermissive(t reflect.Type) bool {
	pkg := t.PkgPath()
	if i := strings.LastIndex(pkg, "/"); i >= 0 {
		pkg = pkg[i+1:]
	}
	_, ok := permissiveStructs[pkg+"."+t.Name()]
	return ok
}

func buildFieldIndex(t reflect.Type) map[string]reflect.StructField {
	out := make(map[string]reflect.StructField)
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name := splitTagName(tag)
		if name == "" {
			name = lowerFirst(f.Name)
		}
		if hasFlag(tag, "inline") && f.Type.Kind() == reflect.Struct {
			for k, v := range buildFieldIndex(f.Type) {
				out[k] = v
			}
			continue
		}
		out[name] = f
	}
	return out
}

func splitTagName(tag string) string {
	for i := 0; i < len(tag); i++ {
		if tag[i] == ',' {
			return tag[:i]
		}
	}
	return tag
}

func hasFlag(tag, flag string) bool {
	for i := 0; i < len(tag); {
		j := i
		for j < len(tag) && tag[j] != ',' {
			j++
		}
		if tag[i:j] == flag {
			return true
		}
		i = j + 1
	}
	return false
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	b := []byte(s)
	if b[0] >= 'A' && b[0] <= 'Z' {
		b[0] += 'a' - 'A'
	}
	return string(b)
}

func joinKey(path, key string) string {
	switch {
	case path == "":
		return key
	case key == "":
		return path
	default:
		return path + "." + key
	}
}

func implementsUnmarshaler(t reflect.Type) bool {
	if t.Implements(yamlUnmarshalerType) {
		return true
	}
	if reflect.PointerTo(t).Implements(yamlUnmarshalerType) {
		return true
	}
	return false
}
