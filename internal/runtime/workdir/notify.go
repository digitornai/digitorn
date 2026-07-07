package workdir

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// NotifyFileChange signals the live workspace notifier that the agent just
// mutated a file in the session workdir, so the daemon pushes a coalesced
// workspace-changes event to the client (Changes panel + file tree). It is the
// shared signal for EVERY file-mutating module — a filesystem.write and a shell
// command that scaffolds files refresh the UI identically. Non-blocking and
// best-effort: it fires only when a notifier, a caller identity, and a real
// workdir all ride on ctx (the agent path); setup / CLI / test calls have none
// and skip silently. Never returns an error: a failed live push must not affect
// the operation that triggered it.
func NotifyFileChange(ctx context.Context) {
	NotifyFileChangePath(ctx)
}

// NotifyFileChangePath is NotifyFileChange with the exact file(s) just written
// (absolute paths). Naming the files lets the daemon push an immediate, reliable
// per-file change — git-status discovery can miss a brand-new file. Each path is
// converted to a workdir-relative, slash-normalised form so it matches what the
// client watches (e.g. "scene.excalidraw").
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
