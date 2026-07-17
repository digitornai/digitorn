package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/ports"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

const workspaceLiveDebounce = 250 * time.Millisecond

const workspaceLiveMaxCoalesce = 750 * time.Millisecond

const workspaceChangesTimeout = 30 * time.Second

type workspaceLive struct {
	rt      ports.RealtimeServer
	builder *sessionstore.EnvelopeBuilder

	changes func(ctx context.Context, workdir string) ([]sessionstore.WorkspaceChangedFile, error)

	previewPush func(ctx context.Context, root string)

	diagnosticsPush func(ctx context.Context, root string, changed []string)
	log             *slog.Logger
	window          time.Duration
	maxWait         time.Duration

	mu   sync.Mutex
	pend map[string]*wsPend
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
func (l *workspaceLive) FileChanged(sessionID, workdir string, paths ...string) {
	if l == nil || sessionID == "" || workdir == "" {
		return
	}
	root := sessionID
	if r, _, isSub := sessionstore.SubAgentSession(sessionID); isSub {
		root = r
	}

	// When the caller named the exact files, push them IMMEDIATELY and reliably
	// — the debounced git-status path below can miss a brand-new file (its first
	// `changes` call bakes it into the baseline, so it shows no diff). This is
	// what makes the embedded preview update in real time on every write.
	l.emitPaths(root, paths)

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

// emitPaths pushes an immediate workspace_changes for the named files, bypassing
// git status entirely — a reliable "re-read this" signal for the preview. Called
// synchronously off the write hot path; the emit itself is best-effort.
func (l *workspaceLive) emitPaths(root string, paths []string) {
	if len(paths) == 0 {
		return
	}
	files := make([]sessionstore.WorkspaceChangedFile, 0, len(paths))
	for _, p := range paths {
		if p == "" {
			continue
		}
		files = append(files, sessionstore.WorkspaceChangedFile{Path: p, Status: "modified"})
	}
	if len(files) == 0 {
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
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := l.rt.Emit(ctx, bridgeNamespace, "session:"+root, "event", env); err != nil && l.log != nil {
		l.log.Debug("workspace live emitPaths failed", "root", root, "err", err.Error())
	}
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
