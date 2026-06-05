package app

import (
	"fmt"
	"regexp"
	"strings"
)

// placeholderRE matches {{env.NAME}} or {{var.NAME}} placeholders.
var placeholderRE = regexp.MustCompile(`\{\{\s*(env|var)\.([A-Za-z_][A-Za-z0-9_]*)\s*\}\}`)

// ResolveVariables expands {{env.X}} and {{var.X}} placeholders in a YAML
// string. Unknown placeholders produce an error.
func ResolveVariables(raw string, envLookup func(string) string, vars map[string]string) (string, error) {
	if envLookup == nil {
		envLookup = func(string) string { return "" }
	}
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(raw, func(match string) string {
		groups := placeholderRE.FindStringSubmatch(match)
		if len(groups) != 3 {
			return match
		}
		switch groups[1] {
		case "env":
			val := envLookup(groups[2])
			if val == "" && firstErr == nil {
				firstErr = fmt.Errorf("env var %q is not set", groups[2])
			}
			return val
		case "var":
			val, ok := vars[groups[2]]
			if !ok && firstErr == nil {
				firstErr = fmt.Errorf("var %q is not declared", groups[2])
			}
			return val
		}
		return match
	})
	if firstErr != nil {
		return "", firstErr
	}
	// Defensive: strip any trailing CR from CRLF-saved files.
	return strings.TrimRight(out, "\r"), nil
}
