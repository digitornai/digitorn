package module

import (
	"strings"

	"gopkg.in/yaml.v3"
)

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
