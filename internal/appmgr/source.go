package appmgr

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// sourceKind enumerates the supported install source flavours.
type sourceKind int

const (
	sourceLocal sourceKind = iota
	sourceHub
	sourceBuiltin
)

func (k sourceKind) String() string {
	switch k {
	case sourceLocal:
		return "local"
	case sourceHub:
		return "hub"
	case sourceBuiltin:
		return "builtin"
	default:
		return "unknown"
	}
}

// fetchInfo carries metadata back from fetchSource. cleanup is a
// no-op for local sources and a tmp-dir RemoveAll for fetched ones.
type fetchInfo struct {
	kind    sourceKind
	source  string // original URI for logging
	cleanup func()
}

// fetchSource resolves an install URI to an on-disk directory that
// contains app.yaml at its root. For local sources the path is used
// in place ; for hub/builtin sources we download/extract into a temp
// dir and the returned cleanup removes it on defer.
func (m *gormManager) fetchSource(ctx context.Context, source, userJWT string) (string, fetchInfo, error) {
	info := fetchInfo{source: source, cleanup: func() {}}
	if source == "" {
		return "", info, fmt.Errorf("%w: empty source", ErrBadSource)
	}

	switch {
	case strings.HasPrefix(source, "hub://"):
		info.kind = sourceHub
		dir, err := m.fetchHub(ctx, source, userJWT)
		if err != nil {
			return "", info, err
		}
		info.cleanup = func() { _ = os.RemoveAll(dir) }
		return dir, info, nil

	case strings.HasPrefix(source, "builtin://"):
		info.kind = sourceBuiltin
		dir, err := m.fetchBuiltin(strings.TrimPrefix(source, "builtin://"))
		if err != nil {
			return "", info, err
		}
		info.cleanup = func() { _ = os.RemoveAll(dir) }
		return dir, info, nil

	default:
		info.kind = sourceLocal
		st, err := os.Stat(source)
		if err != nil {
			return "", info, fmt.Errorf("%w: stat %q: %v", ErrBadSource, source, err)
		}
		if !st.IsDir() {
			return "", info, fmt.Errorf("%w: not a directory: %q", ErrBadSource, source)
		}
		return source, info, nil
	}
}

