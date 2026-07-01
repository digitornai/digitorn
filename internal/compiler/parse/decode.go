package parse

import (
	"fmt"
	"reflect"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
)

// StrictDecode populates target from node using yaml.v3 and reports the
// decode error (if any) on the bag. Unknown-field detection is delegated to
// CheckUnknownFields to avoid double reporting.
func StrictDecode(file string, node *yaml.Node, target any, path string, bag *diagnostic.Bag) error {
	if node == nil {
		return nil
	}
	rv := reflect.ValueOf(target)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return fmt.Errorf("strict decode: target must be a non-nil pointer to struct")
	}
	if rv.Elem().Kind() != reflect.Struct {
		return node.Decode(target)
	}
	if node.Kind != yaml.MappingNode {
		bag.Add(diagnostic.Errorf(diagnostic.CodeUnexpectedType, NodePos(file, node),
			"%sexpected a mapping, got %s", pathPrefix(path), kindName(node.Kind)))
		return nil
	}
	if err := node.Decode(target); err != nil {
		bag.Add(diagnostic.Errorf(diagnostic.CodeWrongType, NodePos(file, node),
			"%sdecode error: %v", pathPrefix(path), err))
	}
	return nil
}

func pathPrefix(path string) string {
	if path == "" {
		return ""
	}
	return path + ": "
}

func kindName(k yaml.Kind) string {
	switch k {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return fmt.Sprintf("unknown(%d)", int(k))
	}
}

// closestMatch is wired by the suggest package at init time. Until wired, it
// returns no suggestion so cycles are avoided.
var closestMatchHook = func(target string, pool []string) (string, bool) { return "", false }

func closestMatch(target string, pool []string) (string, bool) { return closestMatchHook(target, pool) }

func SetClosestMatchHook(fn func(string, []string) (string, bool)) {
	if fn != nil {
		closestMatchHook = fn
	}
}
