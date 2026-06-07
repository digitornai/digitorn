package workdir

import (
	"context"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
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
	n.FileChanged(id.SessionID, pp.Root())
}
