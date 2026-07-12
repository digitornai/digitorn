package filesystem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

var errFuzzyNoMatch = errors.New("no fuzzy path match")

// resolveReadable resolves rel for a tool that needs the file to EXIST
// (read/edit), tolerating an unambiguous near-miss: wrong case, a bare
// basename, a ./ prefix, or the wrong directory. An exact hit is returned
// untouched. A miss with exactly one workspace candidate resolves to it; with
// several, an ambiguity error lists them; with none, rel is returned unchanged
// so the caller emits its own not-found. Never used by write (a miss = create).
func (m *Module) resolveReadable(ctx context.Context, rel string) (abs, actual string, err error) {
	abs, err = m.resolveCtx(ctx, rel)
	if err != nil {
		return "", "", err
	}
	if _, e := os.Stat(abs); e == nil {
		return abs, rel, nil
	} else if !os.IsNotExist(e) {
		return "", "", e
	}
	a, act, ferr := m.fuzzyResolvePath(ctx, rel)
	if ferr == nil {
		return a, act, nil
	}
	if errors.Is(ferr, errFuzzyNoMatch) {
		return abs, rel, nil
	}
	return "", "", ferr
}

// fuzzyResolvePath finds the file rel most plausibly meant. One candidate → its
// absolute path; several → an ambiguity error; none → errFuzzyNoMatch.
func (m *Module) fuzzyResolvePath(ctx context.Context, rel string) (abs, actual string, err error) {
	cands := m.fuzzyFindPath(ctx, rel)
	switch len(cands) {
	case 0:
		return "", "", errFuzzyNoMatch
	case 1:
		a, e := m.resolveCtx(ctx, cands[0])
		if e != nil {
			return "", "", errFuzzyNoMatch
		}
		return a, cands[0], nil
	default:
		show := cands
		if len(show) > 8 {
			show = show[:8]
		}
		return "", "", fmt.Errorf("%s: not found, but %d files match that name — pass the full path, one of: %s",
			rel, len(cands), strings.Join(show, ", "))
	}
}

// fuzzyFindPath scans the workspace for files matching rel, by precedence:
// case-insensitive full path, then path-suffix, then basename. Returns the
// first non-empty tier (deduped) so basename noise never dilutes a better match.
func (m *Module) fuzzyFindPath(ctx context.Context, rel string) []string {
	root, ok := m.globBase(ctx)
	if !ok || root == "" {
		return nil
	}
	want := strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(rel)), "./")
	want = strings.TrimPrefix(want, "/")
	if want == "" {
		return nil
	}
	wantLow := strings.ToLower(want)
	baseLow := strings.ToLower(slashBase(want))

	gi := loadGitignore(root)
	var exactCI, suffix, byBase []string
	const scanCap = 20000
	seen := 0
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(d.Name(), ".") || sindexIgnoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			if r, ok := relUnder(root, path); ok && gi.ignored(r, true) {
				return filepath.SkipDir
			}
			return nil
		}
		if seen++; seen > scanCap {
			return filepath.SkipAll
		}
		r, ok := relUnder(root, path)
		if !ok {
			return nil
		}
		rs := filepath.ToSlash(r)
		if gi.ignored(rs, false) {
			return nil
		}
		switch rl := strings.ToLower(rs); {
		case rl == wantLow:
			exactCI = append(exactCI, rs)
		case strings.HasSuffix(rl, "/"+wantLow):
			suffix = append(suffix, rs)
		case strings.ToLower(slashBase(rs)) == baseLow:
			byBase = append(byBase, rs)
		}
		return nil
	})
	for _, set := range [][]string{exactCI, suffix, byBase} {
		if len(set) > 0 {
			return dedupe(set)
		}
	}
	return nil
}

func slashBase(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, s := range in {
		if _, ok := seen[s]; ok {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}
