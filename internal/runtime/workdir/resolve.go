package workdir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Mode is the app-declared workdir policy (runtime.workdir_mode).
type Mode string

const (
	ModeNone     Mode = "none"     // no workdir; file-path tools are unavailable.
	ModeAuto     Mode = "auto"     // default: managed per-session dir when nothing supplied.
	ModeFixed    Mode = "fixed"    // app pins an absolute path (runtime.workdir).
	ModeRequired Mode = "required" // client MUST supply a workdir at session creation.
)

// NormalizeMode maps an empty / unknown mode to the default (auto).
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

// ErrWorkdirRequired is returned by Resolve when the app declares
// workdir_mode: required but the client supplied no workdir at session
// creation. The server maps it to a 400 {error: "workdir_required"}.
var ErrWorkdirRequired = errors.New("workdir_required")

// Request carries everything needed to resolve a session's workdir at session
// creation. UserWorkdir is the client-supplied path (POST /sessions body);
// FixedPath is the app's runtime.workdir (fixed mode).
type Request struct {
	Mode        Mode
	FixedPath   string
	UserWorkdir string
	AppID       string
	UserID      string
	SessionID   string
	Home        string // base dir; "" → os.UserHomeDir()
}

// Resolve returns the absolute workdir root for a session, creating the managed
// directory ONLY in the default fallback case (no dead directories). Precedence:
//
//  1. UserWorkdir supplied  → validated existing absolute dir, used as-is
//     (no {user_id}/{session} segment — it is the user's own path).
//  2. mode == required      → ErrWorkdirRequired (no session is created).
//  3. mode == fixed         → app's FixedPath, created if missing.
//  4. mode == none          → "" (no workdir).
//  5. otherwise (auto)      → ~/.digitorn/workdirs/{app}/{user}/{session}/,
//     created lazily HERE and nowhere else.
//
// Returns "" with nil error only for ModeNone.
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
	default: // auto
		return ensureManaged(r)
	}
}

// validateUserWorkdir checks a client-supplied workdir: it must be an absolute
// path, and is CREATED if it doesn't exist yet (like fixed mode) so a user can
// point the agent at a fresh folder. Errors if the path exists but is a file,
// or can't be created. Returned canonical (symlinks resolved).
func validateUserWorkdir(p string) (string, error) {
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("workdir %q must be an absolute path", p)
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("workdir %q is not usable: %w", p, err)
	}
	return canonical(p), nil
}

// resolveFixed validates the app's pinned workdir, creating it if missing.
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

// ensureManaged creates and returns the default per-session managed workdir.
// This is the ONLY place a managed directory is created.
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

// safeSeg sanitises one path segment built from a daemon id (app/user/session)
// so a hostile id can't traverse out of the workdirs tree. Strips separators
// and any "." / ".." component; empty result falls back to "_".
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
