package theme

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// prefsFile is the per-user CLI preferences blob, next to the daemon
// credentials. Holds the last-selected theme so it survives restarts.
const prefsFile = ".digitorn/cli.json"

type prefs struct {
	Theme string `json:"theme,omitempty"`
}

func prefsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, prefsFile), nil
}

// Preferred returns the theme saved by a previous SavePreferred, or the
// built-in default when none is set / unreadable / unknown. This is the
// theme the TUI starts with.
func Preferred() *Theme {
	path, err := prefsPath()
	if err != nil {
		return Default()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Default()
	}
	var p prefs
	if json.Unmarshal(b, &p) != nil || p.Theme == "" {
		return Default()
	}
	if t := Get(p.Theme); t != nil {
		return t
	}
	return Default()
}

// SavePreferred records name as the user's theme. Best-effort : a write
// failure (read-only home, etc.) is swallowed — the switch still applies
// for the current session.
func SavePreferred(name string) {
	path, err := prefsPath()
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o700)
	b, err := json.Marshal(prefs{Theme: name})
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}
