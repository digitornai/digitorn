package validate

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/mbathepaul/digitorn/internal/compiler/diagnostic"
	"github.com/mbathepaul/digitorn/internal/compiler/position"
	"github.com/mbathepaul/digitorn/internal/compiler/schema"
	"github.com/mbathepaul/digitorn/pkg/module"
)

func CheckPromptFrontmatter(bundleRoot string, def *schema.AppDefinition, bag *diagnostic.Bag) {
	if bundleRoot == "" {
		return
	}
	declared := declaredVariables(def)
	scanPromptDir(filepath.Join(bundleRoot, "prompts"), "prompt", declared, bag)
	scanPromptDir(filepath.Join(bundleRoot, "skills"), "skill", declared, bag)
}

func declaredVariables(def *schema.AppDefinition) map[string]struct{} {
	out := map[string]struct{}{}
	if def.Dev != nil {
		for k := range def.Dev.Variables {
			out[k] = struct{}{}
		}
	}
	return out
}

func scanPromptDir(dir, kind string, declared map[string]struct{}, bag *diagnostic.Bag) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fm, _, hasFM, err := module.SplitFrontmatter(string(data))
		if err != nil {
			bag.Add(diagnostic.Errorf(diagnostic.CodeYAMLSyntax, position.Pos{File: path},
				"%s frontmatter: %s", kind, err.Error()))
			continue
		}
		if !hasFM {
			continue
		}
		for _, v := range fm.VariablesRequired {
			if !isKnownVariable(v, declared) {
				bag.Add(diagnostic.Errorf(diagnostic.CodeMissingRequired, position.Pos{File: path},
					"%s/%s: variable %q is required by frontmatter but never declared",
					kind, e.Name(), v))
			}
		}
		if fm.MaxTokensEstimate > 200000 {
			bag.Add(diagnostic.Warningf(diagnostic.CodeExperimentalFeature, position.Pos{File: path},
				"%s/%s: max_tokens_estimate %d is unusually large (>200000)",
				kind, e.Name(), fm.MaxTokensEstimate))
		}
	}
}

func isKnownVariable(v string, declared map[string]struct{}) bool {
	if dot := strings.Index(v, "."); dot > 0 {
		switch v[:dot] {
		case "env", "sys", "secret", "app", "var", "prompt", "skill", "behavior", "asset", "asset_b64":
			return true
		}
	}
	_, ok := declared[v]
	return ok
}
