package filesystem

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/digitornai/digitorn/internal/docstore"
	"github.com/digitornai/digitorn/internal/runtime/workdir"
)

// docSyncData is the LSP-like sentinel: after a mutation lands inside a
// fragmented document, it validates + syncs and returns a "sync" payload the
// tool result carries back to the agent — diagnostics included, never silent.
func docSyncData(ctx context.Context, abs string) map[string]any {
	dir, kind := docstore.FindDocDir(abs)
	if kind == "" {
		return nil
	}
	rel := func(p string) string {
		if pp, ok := workdir.PathPolicyFromContext(ctx); ok && pp.HasWorkdir() {
			if r, err := filepath.Rel(pp.Root(), p); err == nil {
				return filepath.ToSlash(r)
			}
		}
		return p
	}
	switch kind {
	case "fragment":
		res, err := docstore.SyncFragments(dir)
		out := map[string]any{"composed": rel(res.ComposedPath), "composed_ok": res.Composed}
		if len(res.Diagnostics) > 0 {
			out["diagnostics"] = res.Diagnostics
		}
		if err != nil && !errors.Is(err, docstore.ErrInvalid) {
			out["error"] = err.Error()
		}
		if res.Composed {
			notifyFileChange(ctx, res.ComposedPath)
		} else {
			out["note"] = "compose refused — the composed document was NOT updated; fix the diagnostics above"
		}
		return out
	case "composed":
		changed, err := docstore.SyncComposed(abs)
		out := map[string]any{
			"decomposed": changed,
			"note":       "this is a fragmented document — prefer editing its fragments under " + rel(dir) + "/",
		}
		if err != nil {
			out["error"] = err.Error()
		}
		return out
	}
	return nil
}
