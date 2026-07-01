package server

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/digitornai/digitorn/internal/ports"
)

// diagnosableExts are the source extensions a built-in language server can answer
// (gopls / pyright / typescript-language-server / rust-analyzer / texlab). A
// changed file outside this set is skipped: no server would respond, and the
// Problems panel never lists non-code files. Apps that declare extra servers
// still work — an unknown extension just yields no diagnostics (a clean clear).
var diagnosableExts = map[string]bool{
	".go": true, ".py": true, ".pyi": true,
	".ts": true, ".tsx": true, ".js": true, ".jsx": true, ".mjs": true, ".cjs": true,
	".rs": true, ".tex": true, ".bib": true,
}

// maxDiagnoseFilesPerPush caps how many changed files are diagnosed in one
// debounced push so a bulk scaffold (hundreds of files at once) can't pin the
// language servers or the push goroutine. Excess files are skipped this round
// and picked up on a later change.
const maxDiagnoseFilesPerPush = 60

// pushDiagnostics runs the lsp module over the changed source files and streams
// the result to the session's Problems panel on the `diagnostics` channel — the
// same ephemeral, bus-bypassing realtime path as the workspace-changes / preview
// pushes (never the durable bus, no transcript bloat). Per file: diagnostics →
// `preview:resource_set`; came back clean → `preview:resource_deleted` so the
// panel drops it. Best-effort by contract: an app without the lsp module, or a
// language-server hiccup, just yields no diagnostics — never fatal, never
// blocking, runs in the debounce timer's own goroutine (off every hot path).
func (d *Daemon) pushDiagnostics(ctx context.Context, root string, changed []string) {
	if d.rt == nil || d.bus == nil || root == "" || len(changed) == 0 {
		return
	}
	state, err := d.sessionStore.State(root)
	if err != nil {
		return
	}
	state.RLock()
	wd := state.Workdir
	state.RUnlock()
	if wd == "" {
		return
	}

	n := 0
	for _, rel := range changed {
		if rel == "" || !diagnosableExts[strings.ToLower(filepath.Ext(rel))] {
			continue
		}
		if n >= maxDiagnoseFilesPerPush {
			break
		}
		n++
		abs := filepath.Join(wd, filepath.FromSlash(rel))
		emitFileDiagnostics(ctx, d.rt, root, rel, d.lspDiagnose(ctx, abs))
	}
}

// emitFileDiagnostics streams one file's diagnostics to the session's Problems
// panel on the `diagnostics` channel: non-empty → `preview:resource_set` with the
// items, empty (file came back clean) → `preview:resource_deleted` so the panel
// drops the entry. The envelope shape is exactly what the web's workspace-module
// reducer consumes (data.channel / data.id / data.payload). Best-effort emit.
func emitFileDiagnostics(ctx context.Context, rt ports.RealtimeServer, root, rel string, items []map[string]any) {
	if rt == nil || root == "" || rel == "" {
		return
	}
	if len(items) == 0 {
		_ = rt.Emit(ctx, bridgeNamespace, "session:"+root, "event", map[string]any{
			"type":       "preview:resource_deleted",
			"session_id": root,
			"channel":    "diagnostics",
			"id":         rel,
		})
		return
	}
	_ = rt.Emit(ctx, bridgeNamespace, "session:"+root, "event", map[string]any{
		"type":       "preview:resource_set",
		"session_id": root,
		"channel":    "diagnostics",
		"id":         rel,
		"payload": map[string]any{
			"items":        items,
			"source_label": "lsp",
		},
	})
}

// lspDiagnose syncs one absolute path to its language server and returns the raw
// diagnostic items ({severity,line,column,message,source,code} — already the
// shape the web's parseDiagnostics consumes). The result is normalised through
// JSON so it works whether the lsp module ran in-proc (typed slice) or in a
// worker (already JSON). Returns nil on any error — diagnostics are advisory.
func (d *Daemon) lspDiagnose(ctx context.Context, abs string) []map[string]any {
	raw, _ := json.Marshal(map[string]any{"path": abs})
	res, err := d.bus.Call(ctx, "lsp", "notify_change", raw)
	if err != nil || !res.Success {
		return nil
	}
	b, err := json.Marshal(res.Data)
	if err != nil {
		return nil
	}
	var out struct {
		Diagnostics []map[string]any `json:"diagnostics"`
	}
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	return out.Diagnostics
}
