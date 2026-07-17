package workdir

import "strings"

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
