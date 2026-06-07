package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/ports"
	"github.com/mbathepaul/digitorn/internal/runtime/sessionstore"
)

// workspaceLiveDebounce is the quiet window a session must go without a file
// write before its pending-changes are recomputed and pushed. It coalesces a
// burst of writes (a multi_edit, an agent rewriting many files) into ONE git
// status + ONE push, so a heavy edit never fans out into a storm of refreshes.
const workspaceLiveDebounce = 250 * time.Millisecond

// workspaceLiveMaxCoalesce caps how long a continuous burst of writes may delay
// a push. Rapid writes still coalesce (one git status per quiet window), but a
// long uninterrupted edit (a scaffold, a multi_edit, parallel writes) flushes at
// least this often instead of only once the burst ends — so the web streams the
// latest state while the agent works, never waiting for the turn to finish.
const workspaceLiveMaxCoalesce = 750 * time.Millisecond

// workspaceChangesTimeout bounds the background git status so a pathological
// repo can never pin a goroutine forever.
const workspaceChangesTimeout = 30 * time.Second

// workspaceLive is the daemon-side FileChangeNotifier (tool.FileChangeNotifier).
// The filesystem module calls FileChanged the instant it finishes a write/edit;
// this debounces per session-root, recomputes the pending changes off the hot
// path, and pushes them straight to the session's realtime room — EPHEMERAL:
// never through the durable bus, so no transcript bloat and no seq burn.
//
// Scale : the map holds an entry only for sessions that wrote in the last
// debounce window (the entry is removed once its refresh fires), so it is
// O(active editors), not O(all sessions). One runtime timer per active editor.
type workspaceLive struct {
	rt      ports.RealtimeServer
	builder *sessionstore.EnvelopeBuilder
	// changes recomputes a session's pending file list (git status on the shadow
	// repo). Injected so tests don't need a real repo, and so it moves to a
	// worker pool later without touching this debouncer.
	changes func(ctx context.Context, workdir string) ([]sessionstore.WorkspaceChangedFile, error)
	// previewPush re-resolves + pushes the live preview URL (web_preview:attached)
	// for the session root, off the same debounced workspace-change signal. nil in
	// tests / when realtime is absent.
	previewPush func(ctx context.Context, root string)
	// diagnosticsPush re-runs the lsp module over the changed files and streams
	// the result to the session's Problems panel (diagnostics channel), off the
	// same debounced signal. nil in tests / when realtime is absent.
	diagnosticsPush func(ctx context.Context, root string, changed []string)
	log             *slog.Logger
	window          time.Duration
	maxWait         time.Duration

	mu   sync.Mutex
	pend map[string]*wsPend // session-root -> in-flight debounce state
}

// wsPend is the per-session-root debounce state. running guards against a
// refresh racing a fresh burst; again records that a write arrived mid-refresh
// so we re-arm exactly once instead of dropping it.
type wsPend struct {
	workdir string
	timer   *time.Timer
	first   time.Time // start of the current coalesce window (for the max-wait cap)
	running bool
	again   bool
}

// newWorkspaceLive builds the notifier from the daemon's realtime handles. nil
// is returned (and wiring skipped) when the realtime stack is absent.
func (d *Daemon) newWorkspaceLive() *workspaceLive {
	if d.rt == nil || d.envelopeBuilder == nil {
		return nil
	}
	return &workspaceLive{
		rt:              d.rt,
		builder:         d.envelopeBuilder,
		changes:         d.workspaceChangedFiles,
		previewPush:     d.pushPreviewSource,
		diagnosticsPush: d.pushDiagnostics,
		log:             d.logger,
		window:          workspaceLiveDebounce,
		maxWait:         workspaceLiveMaxCoalesce,
		pend:            make(map[string]*wsPend),
	}
}

// FileChanged records that the agent mutated a file in workdir for sessionID.
// Non-blocking by contract : it only touches the map and (re)arms a timer — the
// git status and the push happen later, in the timer's own goroutine, so the
// filesystem write that called us returns immediately. A sub-agent write is
// folded onto its session ROOT so the user's session room gets one coalesced
// push for the whole agent tree.
func (l *workspaceLive) FileChanged(sessionID, workdir string) {
	if l == nil || sessionID == "" || workdir == "" {
		return
	}
	root := sessionID
	if r, _, isSub := sessionstore.SubAgentSession(sessionID); isSub {
		root = r
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if p := l.pend[root]; p != nil {
		p.workdir = workdir
		switch {
		case p.running:
			p.again = true // a write landed mid-refresh: refresh once more after.
		case now.Sub(p.first) >= l.maxWait:
			p.timer.Reset(0) // burst longer than the cap: flush now, don't keep deferring.
		default:
			p.timer.Reset(l.window)
		}
		return
	}
	p := &wsPend{workdir: workdir, first: now}
	p.timer = time.AfterFunc(l.window, func() { l.fire(root) })
	l.pend[root] = p
}

// fire runs in the debounce timer's goroutine (off every hot path). It snapshots
// the workdir under the lock, recomputes + pushes OUTSIDE the lock, then either
// re-arms (a burst arrived during the refresh) or drops the entry.
func (l *workspaceLive) fire(root string) {
	l.mu.Lock()
	p := l.pend[root]
	if p == nil {
		l.mu.Unlock()
		return
	}
	p.running = true
	workdir := p.workdir
	l.mu.Unlock()

	defer func() {
		if rec := recover(); rec != nil && l.log != nil {
			l.log.Warn("workspace live push panicked", "root", root, "panic", rec)
		}
		l.mu.Lock()
		p.running = false
		if p.again {
			p.again = false
			p.first = time.Now()    // fresh coalesce window for the post-flush burst
			p.timer.Reset(l.window) // a write arrived during the refresh — re-run.
		} else {
			delete(l.pend, root)
		}
		l.mu.Unlock()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), workspaceChangesTimeout)
	defer cancel()
	files, err := l.changes(ctx, workdir)
	if err != nil {
		if l.log != nil {
			l.log.Warn("workspace live changes failed", "root", root, "err", err.Error())
		}
		return
	}

	ev := sessionstore.Event{
		Type:       sessionstore.EventWorkspaceChanges,
		SessionID:  root,
		TsUnixNano: time.Now().UnixNano(),
		WorkspaceChanges: &sessionstore.WorkspaceChangesPayload{
			Files: files,
			Count: len(files),
		},
	}
	env := l.builder.Build(&ev)
	// Direct to the session room, bypassing the durable bus: this is an
	// ephemeral UI signal, not transcript history. Same isolation as the bridge
	// (one session room only), best-effort.
	if err := l.rt.Emit(ctx, bridgeNamespace, "session:"+root, "event", env); err != nil && l.log != nil {
		l.log.Debug("workspace live emit failed", "root", root, "err", err.Error())
	}

	// Same debounced signal also refreshes the live preview source: re-resolve the
	// built entry and push web_preview:attached so the iframe reloads — no polling.
	if l.previewPush != nil {
		l.previewPush(ctx, root)
	}

	// Same debounced signal feeds the Problems panel: re-diagnose the changed
	// source files and stream LSP errors/warnings on the diagnostics channel.
	if l.diagnosticsPush != nil && len(files) > 0 {
		paths := make([]string, 0, len(files))
		for _, f := range files {
			paths = append(paths, f.Path)
		}
		l.diagnosticsPush(ctx, root, paths)
	}
}

// workspaceChangedFiles recomputes one session's pending changes via the
// workspace module (git status on the shadow repo) and projects the result onto
// the client payload shape. The JSON round-trip keeps it identical whether the
// module runs in-process or, later, in a worker pool.
func (d *Daemon) workspaceChangedFiles(ctx context.Context, workdir string) ([]sessionstore.WorkspaceChangedFile, error) {
	data, err := d.invokeWorkspace(ctx, "changes", workdir, nil)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	var out struct {
		Files []sessionstore.WorkspaceChangedFile `json:"files"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Files, nil
}
