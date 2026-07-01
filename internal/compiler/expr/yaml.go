package expr

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/digitornai/digitorn/internal/compiler/diagnostic"
	"github.com/digitornai/digitorn/internal/compiler/position"
)

// ResolveInTree walks the YAML AST and rewrites every scalar value that
// contains a {{...}} placeholder. When a resolved value parses back as
// structured YAML (sequence / mapping / flow style), the scalar is replaced
// with the parsed subtree so `{{include:fragments/agents.yaml}}` can splice
// a list into a list field.
func ResolveInTree(file string, root *yaml.Node, engine *Engine, bag *diagnostic.Bag) {
	walkNode(file, root, engine, bag)
}

func walkNode(file string, n *yaml.Node, engine *Engine, bag *diagnostic.Bag) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			walkNode(file, c, engine, bag)
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			walkNode(file, n.Content[i], engine, bag)
		}
	case yaml.ScalarNode:
		if !strings.Contains(n.Value, "{{") {
			return
		}
		resolved, err := engine.ResolveString(n.Value)
		if err != nil {
			bag.Add(diagnostic.Errorf(
				codeFor(err),
				position.Pos{File: file, Line: n.Line, Column: n.Column},
				"%v", err,
			))
			return
		}
		spliceResolved(n, resolved)
	}
}

func spliceResolved(n *yaml.Node, resolved string) {
	if structuralYAML(resolved) {
		var sub yaml.Node
		if err := yaml.Unmarshal([]byte(resolved), &sub); err == nil && len(sub.Content) > 0 {
			line, col := n.Line, n.Column
			*n = *sub.Content[0]
			n.Line, n.Column = line, col
			return
		}
	}
	n.Value = resolved
	n.Tag = ""
	n.Style = 0
}

// structuralYAML decides whether a resolved value should splice into the AST
// as a structured subtree (vs. copying as a literal string). Only sequences
// qualify — the canonical use case is {{include:fragments/agents.yaml}}.
// JSON-shaped strings and text starting with '{' stay as strings so they
// can populate scalar fields.
func structuralYAML(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return false
	}
	if strings.Contains(t, "{{") {
		return false
	}
	return strings.HasPrefix(t, "- ") ||
		strings.HasPrefix(t, "-\n") ||
		strings.HasPrefix(t, "[")
}

func codeFor(err error) diagnostic.Code {
	s := err.Error()
	switch {
	case strings.Contains(s, "unknown namespace"):
		return diagnostic.CodeUnknownNamespace
	case strings.Contains(s, "exceeded max depth"):
		return diagnostic.CodePlaceholderCycle
	case strings.Contains(s, "prompts/"):
		return diagnostic.CodeMissingPromptFile
	case strings.Contains(s, "skills/"):
		return diagnostic.CodeMissingSkillFile
	case strings.Contains(s, "behavior/"):
		return diagnostic.CodeMissingBehaviorFile
	case strings.Contains(s, "asset"):
		return diagnostic.CodeMissingAssetFile
	case strings.Contains(s, "include:"):
		return diagnostic.CodeMissingIncludeFile
	case strings.Contains(s, "unresolved"):
		return diagnostic.CodeMissingEnvVar
	default:
		return diagnostic.CodeBadPlaceholderSyntax
	}
}
