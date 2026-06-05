// Package parse loads YAML into a positional AST and provides Node helpers.
package parse

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/position"
)

type ParsedFile struct {
	File     string
	Source   []byte
	Root     *yaml.Node
	Document *yaml.Node
}

func ParseFile(path string) (*ParsedFile, *diagnostic.Bag, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(path, data)
}

func Parse(file string, data []byte) (*ParsedFile, *diagnostic.Bag, error) {
	bag := diagnostic.NewBag()
	root := &yaml.Node{}
	if err := yaml.Unmarshal(data, root); err != nil {
		switch e := err.(type) {
		case *yaml.TypeError:
			for _, msg := range e.Errors {
				bag.Add(diagnostic.Errorf(diagnostic.CodeYAMLSyntax, position.Pos{File: file}, "%s", msg))
			}
		default:
			bag.Add(diagnostic.Errorf(diagnostic.CodeYAMLSyntax, position.Pos{File: file}, "%v", err))
		}
		return &ParsedFile{File: file, Source: data, Root: root}, bag, nil
	}

	if root.Kind == 0 || len(root.Content) == 0 {
		bag.Add(diagnostic.Errorf(diagnostic.CodeEmptyDocument,
			position.Pos{File: file, Line: 1, Column: 1}, "YAML document is empty"))
		return &ParsedFile{File: file, Source: data, Root: root}, bag, nil
	}
	if len(root.Content) > 1 {
		bag.Add(diagnostic.Errorf(diagnostic.CodeMultipleDocuments,
			NodePos(file, root.Content[1]), "multiple YAML documents are not supported"))
	}

	return &ParsedFile{File: file, Source: data, Root: root, Document: root.Content[0]}, bag, nil
}

func NodePos(file string, n *yaml.Node) position.Pos {
	if n == nil {
		return position.Pos{}
	}
	return position.Pos{File: file, Line: n.Line, Column: n.Column}
}

func IsMapping(n *yaml.Node) bool  { return n != nil && n.Kind == yaml.MappingNode }
func IsSequence(n *yaml.Node) bool { return n != nil && n.Kind == yaml.SequenceNode }
func IsScalar(n *yaml.Node) bool   { return n != nil && n.Kind == yaml.ScalarNode }

func FindKey(mapping *yaml.Node, key string) (keyNode, valueNode *yaml.Node, ok bool) {
	if !IsMapping(mapping) {
		return nil, nil, false
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		k := mapping.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode && k.Value == key {
			return k, mapping.Content[i+1], true
		}
	}
	return nil, nil, false
}

func MappingKeys(mapping *yaml.Node) []string {
	if !IsMapping(mapping) {
		return nil
	}
	out := make([]string, 0, len(mapping.Content)/2)
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		k := mapping.Content[i]
		if k != nil && k.Kind == yaml.ScalarNode {
			out = append(out, k.Value)
		}
	}
	return out
}

func SequenceItems(seq *yaml.Node) []*yaml.Node {
	if !IsSequence(seq) {
		return nil
	}
	out := make([]*yaml.Node, len(seq.Content))
	copy(out, seq.Content)
	return out
}
