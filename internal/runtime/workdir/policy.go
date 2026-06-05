// Package workdir implements the agent working-directory model and the
// PathPolicy primitive that confines every agent-facing file operation to it.
//
// One concept: the workdir is the single real directory the agent works in
// (there is no separate in-memory "workspace"). One primitive: PathPolicy,
// carried per session on the dispatch context and enforced at the single tool
// chokepoint, so confinement is structural — a module cannot forget it.
//
// PathPolicy is the application-layer (Layer 1) confinement: strong for the
// modules whose every open() the daemon owns (filesystem, workspace). It is
// NOT an OS jail — a shell command body or a subprocess can still escape; that
// needs OS-level sandboxing (Landlock / container) which is a separate layer.
package workdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// PathPolicy confines a raw, agent-supplied path to the session's workdir.
// The zero value (Root == "") denies every relative path and allows no
// absolute path — i.e. "this agent has no workdir, no file access" — while
// still applying the daemon-secret denylist. Build one with NewPolicy so the
// roots are canonicalised (symlinks resolved) for like-for-like comparison.
type PathPolicy struct {
	// root is the absolute, symlink-resolved workdir. "" = no workdir.
	root string
	// allowedExtra are additional absolute, symlink-resolved roots the agent
	// may reach (capabilities.constraints.allowed_paths). Always confined; the
	// secret denylist still applies inside them.
	allowedExtra []string
	// unrestricted lifts the workdir confinement entirely (constraints.
	// unrestricted) — for trusted apps. The daemon-secret denylist is the ONE
	// thing it never lifts.
	unrestricted bool
	// denied are absolute, symlink-resolved files/dirs that are ALWAYS rejected
	// (master key, credentials, db, kv/sessions/state/logs, ~/.claude creds),
	// even when unrestricted.
	denied []string
}

// Options configures NewPolicy. Home is injectable so tests don't touch the
// real ~/.digitorn; empty Home falls back to os.UserHomeDir().
type Options struct {
	Root         string
	AllowedExtra []string
	Unrestricted bool
	Home         string
}

// NewPolicy builds a PathPolicy, canonicalising every root (resolving symlinks
// where the path exists) so containment checks compare resolved-against-
// resolved. Non-existent roots are kept as cleaned absolute paths (the workdir
// may not exist yet on a fresh session).
func NewPolicy(opts Options) PathPolicy {
	p := PathPolicy{unrestricted: opts.Unrestricted}
	if opts.Root != "" {
		p.root = canonical(opts.Root)
	}
	for _, e := range opts.AllowedExtra {
		if e = strings.TrimSpace(e); e != "" {
			p.allowedExtra = append(p.allowedExtra, canonical(expandHome(e, opts.Home)))
		}
	}
	p.denied = daemonSecretPaths(opts.Home)
	return p
}

// Root returns the canonical workdir root ("" = none). Surfaced for the
// system-prompt injection and observability.
func (p PathPolicy) Root() string { return p.root }

// HasWorkdir reports whether a confining workdir is set.
func (p PathPolicy) HasWorkdir() bool { return p.root != "" }

// Enforce validates raw and returns the absolute, symlink-resolved path the
// caller may use, or an error if it escapes the workdir / hits a daemon secret.
// A relative path is rebased onto the workdir; an absolute path is accepted
// only if it resolves inside the workdir (or an allowed-extra root). The
// daemon-secret denylist is checked first and is absolute.
func (p PathPolicy) Enforce(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("path must not be empty")
	}

	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		if p.root == "" {
			return "", fmt.Errorf("no workdir: cannot resolve relative path %q", raw)
		}
		abs = filepath.Clean(filepath.Join(p.root, raw))
	}

	resolved := resolveExistingPrefix(abs)

	// Daemon secrets are rejected ALWAYS — before unrestricted, before any
	// containment check. This is the one rule nothing lifts.
	if p.isDenied(resolved) {
		return "", fmt.Errorf("path %q is a protected daemon secret", raw)
	}
	if p.unrestricted {
		return resolved, nil
	}
	if p.root != "" && within(p.root, resolved) {
		return resolved, nil
	}
	for _, extra := range p.allowedExtra {
		if within(extra, resolved) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("path %q escapes the workdir", raw)
}

// IsAllowed reports whether raw passes Enforce.
func (p PathPolicy) IsAllowed(raw string) bool {
	_, err := p.Enforce(raw)
	return err == nil
}

func (p PathPolicy) isDenied(resolved string) bool {
	for _, d := range p.denied {
		if resolved == d || within(d, resolved) {
			return true
		}
	}
	return false
}

// within reports whether path is root itself or lives under it. Both must be
// clean absolute paths. Uses filepath.Rel so it is case/sep-correct per OS.
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonical returns the cleaned absolute form of p with symlinks resolved on
// the deepest existing ancestor (so a not-yet-created workdir still gets a
// stable, symlink-free root for comparison).
func canonical(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	return resolveExistingPrefix(abs)
}

// resolveExistingPrefix resolves symlinks on the deepest existing ancestor of
// abs, then re-joins the remaining (non-existent) tail. This catches a planted
// symlink in a parent directory even when the final component doesn't exist
// yet (the create-then-escape case). Ported from the filesystem module, now
// the single canonical implementation.
func resolveExistingPrefix(abs string) string {
	cur := abs
	var tail []string
	for {
		if real, err := filepath.EvalSymlinks(cur); err == nil {
			if len(tail) == 0 {
				return real
			}
			parts := append([]string{real}, reversed(tail)...)
			return filepath.Join(parts...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			// Reached the root without finding an existing ancestor.
			return abs
		}
		tail = append(tail, filepath.Base(cur))
		cur = parent
	}
}

func reversed(s []string) []string {
	out := make([]string, len(s))
	for i, v := range s {
		out[len(s)-1-i] = v
	}
	return out
}

// daemonSecretPaths returns the absolute, symlink-resolved set of files/dirs an
// agent may NEVER touch, even under unrestricted. Mirrors the reference
// daemon's denylist (security/workdir-sandbox docs).
func daemonSecretPaths(home string) []string {
	if home == "" {
		if h, err := os.UserHomeDir(); err == nil {
			home = h
		}
	}
	if home == "" {
		return nil
	}
	dg := filepath.Join(home, ".digitorn")
	raw := []string{
		filepath.Join(dg, "master.key"),
		filepath.Join(dg, "server.key"),
		filepath.Join(dg, "credentials.json"),
		filepath.Join(dg, "digitorn.db"),
		filepath.Join(dg, "kv"),
		filepath.Join(dg, "sessions"),
		filepath.Join(dg, "state"),
		filepath.Join(dg, "logs"),
		filepath.Join(home, ".claude", ".credentials.json"),
	}
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		out = append(out, canonical(r))
	}
	return out
}

// expandHome expands a leading "~" to home. Used for allowed_paths like
// "~/.cache".
func expandHome(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~"+string(filepath.Separator)) || strings.HasPrefix(p, "~/") {
		if home == "" {
			if h, err := os.UserHomeDir(); err == nil {
				home = h
			}
		}
		if home != "" {
			rest := strings.TrimPrefix(strings.TrimPrefix(p, "~"), string(filepath.Separator))
			rest = strings.TrimPrefix(rest, "/")
			return filepath.Join(home, rest)
		}
	}
	return p
}
