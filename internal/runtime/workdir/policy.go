package workdir

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type PathPolicy struct {
	root string
	allowedExtra []string
	unrestricted bool
	denied []string
}

type Options struct {
	Root         string
	AllowedExtra []string
	Unrestricted bool
	Home         string
}

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

func (p PathPolicy) Root() string { return p.root }

func (p PathPolicy) HasWorkdir() bool { return p.root != "" }

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

func canonical(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		abs = filepath.Clean(p)
	}
	return resolveExistingPrefix(abs)
}

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
