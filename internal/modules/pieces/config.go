package pieces

import (
	"os"
	"path/filepath"
	"runtime"
)

type Config struct {
	StaticAuth map[string]AuthConfig `json:"static_auth,omitempty" yaml:"static_auth,omitempty"`
}

type AuthConfig struct {
	Type         string            `json:"type" yaml:"type"`
	Value        string            `json:"value,omitempty" yaml:"value,omitempty"`
	Fields       map[string]string `json:"fields,omitempty" yaml:"fields,omitempty"`
	AccessToken  string            `json:"accessToken,omitempty" yaml:"accessToken,omitempty"`
	RefreshToken string            `json:"refreshToken,omitempty" yaml:"refreshToken,omitempty"`
	TokenType    string            `json:"tokenType,omitempty" yaml:"tokenType,omitempty"`
	Username     string            `json:"username,omitempty" yaml:"username,omitempty"`
	Password     string            `json:"password,omitempty" yaml:"password,omitempty"`
}

func bridgeBinaryName() string {
	if runtime.GOOS == "windows" {
		return "digitorn-ap-bridge.exe"
	}
	return "digitorn-ap-bridge"
}

func defaultBridgePath() string {
	exe, err := os.Executable()
	if err != nil {
		return bridgeBinaryName()
	}
	return filepath.Join(filepath.Dir(exe), bridgeBinaryName())
}

func defaultPiecesDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".digitorn", "pieces")
	}
	return filepath.Join(home, ".digitorn", "pieces")
}

const defaultTriggerPort = 9234
