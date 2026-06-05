//go:build windows

package bash

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// enrichPath augments the inherited process PATH with any directories from the
// PERSISTED Windows PATH (HKLM machine + HKCU user) that aren't already there.
// The daemon's process PATH depends entirely on HOW it was launched : a Windows
// service, a thin/system context, or a shell started before `python`/`node`
// were added to PATH all yield an incomplete PATH — and the agent's shell then
// can't resolve `python` (it's typically on the USER PATH, not the machine one),
// forcing absolute paths. Merging the registry PATH makes tool resolution
// independent of launch context. Process PATH keeps priority (kept at the front);
// dedup is case- and trailing-slash-insensitive. Best-effort : a registry read
// error leaves the value unchanged.
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
	add(current) // inherited process PATH wins ordering
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
	// PATH is usually REG_EXPAND_SZ (`%SystemRoot%\…`) — expand the variables.
	if exp, err := registry.ExpandString(v); err == nil {
		return exp
	}
	return v
}
