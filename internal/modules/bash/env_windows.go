//go:build windows

package bash

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

func enrichPath(current string) string {
	seen := map[string]bool{}
	var dirs []string
	add := func(p string) {
		for _, d := range strings.Split(p, ";") {
			d = strings.TrimSpace(d)
			if d == "" {
				continue
			}
			key := strings.ToLower(strings.TrimRight(d, `\`))
			if seen[key] {
				continue
			}
			seen[key] = true
			dirs = append(dirs, d)
		}
	}
	add(current)
	add(readRegistryPath(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`))
	add(readRegistryPath(registry.CURRENT_USER, `Environment`))
	return strings.Join(dirs, ";")
}

func readRegistryPath(root registry.Key, path string) string {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	v, _, err := k.GetStringValue("Path")
	if err != nil {
		return ""
	}
	if exp, err := registry.ExpandString(v); err == nil {
		return exp
	}
	return v
}
