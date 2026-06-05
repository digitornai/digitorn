package module

import (
	"strings"

	"gopkg.in/yaml.v3"
)

// PromptFrontmatter is the optional YAML preamble parsed from a prompt or skill
// file. It declares which variables the body uses and its expected size so the
// compiler can lint against missing variables and warn on oversized prompts.
type PromptFrontmatter struct {
	ID                string         `yaml:"id,omitempty"`
	Description       string         `yaml:"description,omitempty"`
	VariablesRequired []string       `yaml:"variables_required,omitempty"`
	VariablesOptional []string       `yaml:"variables_optional,omitempty"`
	MaxTokensEstimate int            `yaml:"max_tokens_estimate,omitempty"`
	Tags              []string       `yaml:"tags,omitempty"`
	Locale            string         `yaml:"locale,omitempty"`
	Author            string         `yaml:"author,omitempty"`
	Meta              map[string]any `yaml:",inline"`
}

// SplitFrontmatter splits a markdown source into (frontmatter, body). The
// frontmatter is delimited by a leading `---\n` and a closing `---\n` (or
// `---\r\n`). When no frontmatter is present, FM is the zero value and body
// equals src.
func SplitFrontmatter(src string) (fm PromptFrontmatter, body string, hasFM bool, err error) {
	if !strings.HasPrefix(src, "---\n") && !strings.HasPrefix(src, "---\r\n") {
		return PromptFrontmatter{}, src, false, nil
	}
	after := strings.TrimPrefix(strings.TrimPrefix(src, "---\r\n"), "---\n")
	end := findClosingDelim(after)
	if end < 0 {
		return PromptFrontmatter{}, src, false, nil
	}
	header := after[:end]
	body = strings.TrimPrefix(strings.TrimPrefix(after[end:], "---\r\n"), "---\n")
	if err := yaml.Unmarshal([]byte(header), &fm); err != nil {
		return PromptFrontmatter{}, body, true, err
	}
	return fm, body, true, nil
}

// findClosingDelim returns the byte offset of the next "---\n" (or "---\r\n")
// on its own line, or -1 if none is found.
func findClosingDelim(s string) int {
	lines := strings.SplitAfter(s, "\n")
	offset := 0
	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\r\n")
		if trimmed == "---" {
			return offset
		}
		offset += len(line)
	}
	return -1
}
