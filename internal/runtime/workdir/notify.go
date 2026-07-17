package workdir

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

func NotifyFileChange(ctx context.Context) {
	NotifyFileChangePath(ctx)
}

func NotifyFileChangePath(ctx context.Context, absPaths ...string) {
	n, ok := tool.FileChangeNotifierFromContext(ctx)
	if !ok || n == nil {
		return
	}
	id, ok := tool.IdentityFromContext(ctx)
	if !ok || id.SessionID == "" {
		return
	}
	pp, ok := PathPolicyFromContext(ctx)
	if !ok || !pp.HasWorkdir() {
		return
	}
	root := pp.Root()
	rels := make([]string, 0, len(absPaths))
	for _, ap := range absPaths {
		if ap == "" {
			continue
		}
		r, err := filepath.Rel(root, ap)
		if err != nil || strings.HasPrefix(r, "..") {
			continue
		}
		rels = append(rels, filepath.ToSlash(r))
	}
	n.FileChanged(id.SessionID, root, rels...)
}
