package parse

import (
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/position"
)

// LookupNode navigates a YAML AST by a dotted path. Integer segments index
// into sequences; identifier segments look up mapping keys.
//
// Returns nil if any segment cannot be resolved.
func LookupNode(root *yaml.Node, path string) *yaml.Node {
	cur := root
	if cur != nil && cur.Kind == yaml.DocumentNode && len(cur.Content) > 0 {
		cur = cur.Content[0]
	}
	if path == "" {
		return cur
	}
	for _, seg := range strings.Split(path, ".") {
		if cur == nil {
			return nil
		}
		if n, err := strconv.Atoi(seg); err == nil {
			if cur.Kind != yaml.SequenceNode || n < 0 || n >= len(cur.Content) {
				return nil
			}
			cur = cur.Content[n]
			continue
		}
		_, val, ok := FindKey(cur, seg)
		if !ok {
			return nil
		}
		cur = val
	}
	return cur
}

func LookupPos(file string, root *yaml.Node, path string) position.Pos {
	n := LookupNode(root, path)
	if n == nil {
		return position.Pos{File: file}
	}
	return position.Pos{File: file, Line: n.Line, Column: n.Column}
}
