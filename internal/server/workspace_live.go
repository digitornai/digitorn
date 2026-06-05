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
	log     *slog.Logger
	window  time.Duration

	mu   sync.Mutex
	pend map[string]*wsPend // session-root -> in-flight debounce state
}

// wsPend is the per-session-root debounce state. running guards against a
// refresh racing a fresh burst; again records that a write arrived mid-refresh
// so we re-arm exactly once instead of dropping it.
type wsPend struct {
	workdir string
	timer   *time.Timer
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
		rt:      d.rt,
		builder: d.envelopeBuilder,
		changes: d.workspaceChangedFiles,
		log:     d.logger,
		window:  workspaceLiveDebounce,
		pend:    make(map[string]*wsPend),
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
	if p := l.pend[root]; p != nil {
		p.workdir = workdir
		if p.running {
			p.again = true // a write landed mid-refresh: refresh once more after.
		} else {
			p.timer.Reset(l.window)
		}
		return
	}
	p := &wsPend{workdir: workdir}
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
