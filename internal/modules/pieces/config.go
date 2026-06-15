package pieces

import (
	"os"
	"path/filepath"
	"runtime"
)

// Config is the per-app configuration block for the pieces module.
// Declared under tools.modules.pieces.config in app YAML.
type Config struct {
	// StaticAuth declares per-piece credentials directly in app config.
	// Useful for trusted internal apps. Format: map[pieceID]AuthConfig.
	// Per-user credentials from the installed pieces store take precedence.
	StaticAuth map[string]AuthConfig `json:"static_auth,omitempty" yaml:"static_auth,omitempty"`
}

// AuthConfig carries inline credentials for a piece in app-config.
type AuthConfig struct {
	Type   string            `json:"type"` // "secret_text", "custom", "oauth2", "basic", "none"
	Value  string            `json:"value,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// bridgeBinaryName returns the expected file name of the bridge binary for the
// current OS.
func bridgeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "digitorn-ap-bridge.exe"
	}
	return "digitorn-ap-bridge"
}

// defaultBridgePath returns the default bridge binary location: next to the
// running executable.
func defaultBridgePath() string {
	exe, err := os.Executable()
	if err != nil {
		return bridgeBinaryName()
	}
	return filepath.Join(filepath.Dir(exe), bridgeBinaryName())
}

// defaultPiecesDir returns the default pieces directory (~/.digitorn/pieces).
func defaultPiecesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".digitorn", "pieces")
	}
	return filepath.Join(home, ".digitorn", "pieces")
}

// defaultTriggerPort is the HTTP port the bridge's trigger server listens on.
const defaultTriggerPort = 9234
