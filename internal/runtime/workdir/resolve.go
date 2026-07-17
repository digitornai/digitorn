package workdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Mode string

const (
	ModeNone     Mode = "none"
	ModeAuto     Mode = "auto"
	ModeFixed    Mode = "fixed"
	ModeRequired Mode = "required"
)

func NormalizeMode(s string) Mode {
	switch Mode(strings.TrimSpace(s)) {
	case ModeNone:
		return ModeNone
	case ModeFixed:
		return ModeFixed
	case ModeRequired:
		return ModeRequired
	default:
		return ModeAuto
	}
}

var ErrWorkdirRequired = errors.New("workdir_required")

type Request struct {
	Mode        Mode
	FixedPath   string
	UserWorkdir string
	AppID       string
	UserID      string
	SessionID   string
	Home        string
}

func Resolve(r Request) (string, error) {
	if uw := strings.TrimSpace(r.UserWorkdir); uw != "" {
		return validateUserWorkdir(uw)
	}
	switch r.Mode {
	case ModeRequired:
		return "", ErrWorkdirRequired
	case ModeFixed:
		return resolveFixed(r.FixedPath)
	case ModeNone:
		return "", nil
	default:
		return ensureManaged(r)
	}
}

func validateUserWorkdir(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("workdir %q must be an absolute path", p)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("workdir %q is not usable: %w", p, err)
	}
	return canonical(p), nil
}

func resolveFixed(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("workdir_mode is fixed but runtime.workdir is empty")
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("fixed workdir %q must be an absolute path", p)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("create fixed workdir %q: %w", p, err)
	}
	return canonical(p), nil
}

func ensureManaged(r Request) (string, error) {
	home := r.Home
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home: %w", err)
		}
		home = h
	}
	dir := filepath.Join(home, ".digitorn", "workdirs",
		safeSeg(r.AppID), safeSeg(r.UserID), safeSeg(r.SessionID))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create managed workdir %q: %w", dir, err)
	}
	return canonical(dir), nil
}

func safeSeg(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.Trim(s, ". ")
	if s == "" {
		return "_"
	}
	return s
}
