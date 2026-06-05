package workdir

import "strings"

// EnforceArgs validates and REWRITES the path-typed args in place against the
// policy. For each key in pathKeys whose arg is a non-empty string, it runs
// Enforce and replaces the arg with the resolved absolute path — so the module
// receives an already-confined path and can never be handed one outside the
// workdir. Returns the first escape error (the caller turns it into a blocked
// tool outcome).
//
// Absent / empty / non-string args are left untouched: an omitted path lets
// the module apply its default (which resolves to the workdir root, still
// confined), and a non-string is not a path to enforce.
func EnforceArgs(p PathPolicy, args map[string]any, pathKeys ...string) error {
	if args == nil || len(pathKeys) == 0 {
		return nil
	}
	for _, k := range pathKeys {
		v, ok := args[k]
		if !ok {
			continue
		}
		s, ok := v.(string)
		if !ok || strings.TrimSpace(s) == "" {
			continue
		}
		abs, err := p.Enforce(s)
		if err != nil {
			return err
		}
		args[k] = abs
	}
	return nil
}
