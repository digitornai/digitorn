// Package background implements the BackgroundManager contract
// from internal/runtime/context/meta : a per-session table of
// asynchronously running tool calls.
//
// Each task is launched as a goroutine that calls a wrapped
// ToolDispatcher (so the security gates + audit row apply exactly
// as for a foreground call). Status is updated atomically.
// Cancellation propagates via per-task context.
//
// Concurrency model :
//
//   - One Manager per daemon ; safe for concurrent use across
//     sessions (sync.Map sharded by session id).
//   - Per-session task tables are mu-guarded slices so list/status
//     iteration is consistent under concurrent launches.
//   - Goroutines own the task's terminal status — they're the only
//     writers after launch. Readers (Status/List) use a snapshot.
//
// Limits :
//
//   - MaxTasksPerSession caps the per-session task table to
//     prevent runaway launches. Default 100.
//   - Tasks older than RetainCompleted are reaped on next List call
//     so a hot session doesn't accumulate stale entries forever.
package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/runtime"
	"github.com/digitornai/digitorn/internal/runtime/context/meta"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

// ErrSignalINT and ErrSignalTERM are context causes set by Signal() to
// transport the requested OS signal through the context to the bash module,
// which reads context.Cause() to choose the right syscall instead of SIGKILL.
var (
	ErrSignalINT  = errors.New("background: SIGINT requested")
	ErrSignalTERM = errors.New("background: SIGTERM requested")
)

// EventSink is the minimal slice of the session store the Manager needs
// to publish background-task lifecycle events. Injected via AttachSink so
// the manager stays decoupled from the concrete store (and tests can use
// a fake). nil = no lifecycle events (the agent still gets the next-turn
// auto-notification ; only the live client view is skipped).
type EventSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

// Waker proactively schedules an agent turn for a session. Injected via
// AttachWaker so the manager stays decoupled from the daemon's turn
// orchestration. When a task finishes the manager calls WakeSession so the
// agent is notified instantly instead of only at its next user-driven turn.
// nil = no proactive wake (the completion notification still waits in the
// queue for the next turn). The implementation MUST be non-blocking and
// serialize turns per session.
type Waker interface {
	WakeSession(appID, sessionID, userID string)
}

// Manager is the production in-process BackgroundManager. Construct
// via New(); use AttachDispatcher to plug in the runtime's
// ToolDispatcher (the same one foreground calls use, so audit and
// security gates are identical).
type Manager struct {
	dispatcher runtime.ToolDispatcher

	// sink publishes background-task lifecycle events. nil = no events.
	sink EventSink

	// waker proactively schedules an agent turn when a task finishes.
	// nil = no proactive wake (notification waits for the next turn).
	waker Waker

	// MaxTasksPerSession caps the per-session table size. 0 = use
	// DefaultMaxTasks. Production wires this from config.
	MaxTasksPerSession int

	// RetainCompleted bounds how long completed/errored/cancelled
	// tasks stay in the table after they finish. 0 = use
	// DefaultRetain. Production wires from config.
	RetainCompleted time.Duration

	// nowFn lets tests pin the clock.
	nowFn func() time.Time

	sessions sync.Map // map[string]*sessionTable

	// pending stores completed task notifications waiting to be
	// injected into the next turn of their session. The runtime
	// drains this at turn_start time per
	// docs-site/language/04c-primitives.md "Auto-notification" :
	//
	//     [BACKGROUND TASK COMPLETED] task_id=... tool=... elapsed=...s
	//
	// Keyed by sessionID. Bounded indirectly by per-session task
	// cap × retain ; the notification slice is drained on each
	// DrainNotifications call.
	notMu         sync.Mutex
	notifications map[string][]CompletionNotification
}

// CompletionNotification is what the runtime injects as a system
// message at the next turn start for each completed background
// task.
type CompletionNotification struct {
	TaskID    string
	ToolName  string
	ElapsedMs int64
	Status    string // "completed" | "errored" | "cancelled"
	Output    string // captured stdout/stderr — the WHY behind a failure
}

// Message renders the notification in the doc-defined format :
//
//	[BACKGROUND TASK COMPLETED] task_id=a1b2c3d4 tool=database.sql elapsed=12.3s
//
// On errored / cancelled tasks the prefix is adjusted so the
// agent treats failure differently. Per the doc, the elapsed is
// rendered with one decimal in seconds.
func (n CompletionNotification) Message() string {
	prefix := "[BACKGROUND TASK COMPLETED]"
	switch n.Status {
	case "errored":
		prefix = "[BACKGROUND TASK FAILED]"
	case "cancelled":
		prefix = "[BACKGROUND TASK CANCELLED]"
	case "pattern_matched":
		// Async pattern-wake: task is still running but the agent's wait condition
		// has been met. The agent can resume its work immediately.
		prefix = "[BACKGROUND TASK READY]"
	}
	msg := fmt.Sprintf("%s task_id=%s tool=%s elapsed=%.1fs",
		prefix, n.TaskID, n.ToolName, float64(n.ElapsedMs)/1000)
	// Always include output for pattern_matched and errors; suppress for clean
	// completions to keep context tidy.
	if out := strings.TrimSpace(n.Output); out != "" && n.Status != "completed" {
		const max = 2000
		if len(out) > max {
			out = "…(truncated)…\n" + out[len(out)-max:]
		}
		msg += "\noutput:\n" + out
	}
	return msg
}

// watchPattern polls the task's live log every 300 ms until the
// notifyWhen pattern appears or the task finishes. On first match it
// fires exactly once (sync.Once): enqueues a [BACKGROUND TASK READY]
// notification and proactively wakes the agent session.
func (m *Manager) watchPattern(t *task) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			// Task ended — do one final check in case the last bytes arrived
			// between the last tick and close(t.done).
			if strings.Contains(t.live.tail(), t.notifyWhen) {
				m.firePatternNotification(t)
			}
			return
		case <-ticker.C:
			if strings.Contains(t.live.tail(), t.notifyWhen) {
				m.firePatternNotification(t)
				return
			}
		}
	}
}

// firePatternNotification enqueues a pattern_matched CompletionNotification
// and wakes the agent. sync.Once guarantees it fires at most once per task
// even if the pattern appears many times or there is a race between the
// ticker and the task-end check.
func (m *Manager) firePatternNotification(t *task) {
	t.notifyOnce.Do(func() {
		elapsedMs := (time.Now().UnixNano() - t.startedAt*int64(time.Second)) / int64(time.Millisecond)
		if elapsedMs < 0 {
			elapsedMs = 0
		}
		tail := t.live.tailLines(20)
		m.enqueueNotification(t.sessionID, CompletionNotification{
			TaskID:    t.id,
			ToolName:  t.name,
			ElapsedMs: elapsedMs,
			Status:    "pattern_matched",
			Output: fmt.Sprintf(
				"Pattern %q detected in live output. Task is STILL RUNNING — use task_id=%q to monitor or cancel.\nRecent output:\n%s",
				t.notifyWhen, t.id, tail),
		})
		if m.waker != nil {
			m.waker.WakeSession(t.appID, t.sessionID, t.userID)
		}
	})
}

// Defaults applied when fields are zero.
const (
	DefaultMaxTasks = 100
	DefaultRetain   = 30 * time.Minute
)

// New constructs a Manager. Until AttachDispatcher is called,
// Launch returns "dispatcher not attached".
func New() *Manager {
	return &Manager{
		nowFn:         time.Now,
		notifications: map[string][]CompletionNotification{},
	}
}

// AttachDispatcher wires the runtime dispatcher that runs every
// launched task. Must be called once at bootstrap before any
// Launch fires.
func (m *Manager) AttachDispatcher(d runtime.ToolDispatcher) {
	m.dispatcher = d
}

// AttachSink wires the event sink the manager publishes lifecycle events
// to. Optional ; nil leaves the live client view disabled (the agent
// still gets the next-turn auto-notification). Call once at bootstrap.
func (m *Manager) AttachSink(s EventSink) {
	m.sink = s
}

// AttachWaker wires the proactive turn scheduler. Optional ; nil means a
// finished task's notification simply waits for the next user-driven turn.
// Call once at bootstrap.
func (m *Manager) AttachWaker(w Waker) {
	m.waker = w
}

// sessionTable is the per-session task registry.
type sessionTable struct {
	mu    sync.Mutex
	tasks map[string]*task
}

// task is the in-memory state of one background task. Fields are
// either set at construction or updated under mu by the running
// goroutine.
type task struct {
	id        string
	name      string
	args      map[string]any
	sessionID string
	appID     string
	userID    string
	agentID   string
	state     atomic.Value // string : "running" | "completed" | "errored" | "cancelled"
	startedAt int64
	endedAt   atomic.Int64
	result    atomic.Value // any
	errMsg    atomic.Value // string
	live      *liveLog     // streamed output tail, readable while still running
	waiters   atomic.Int32 // # of in-flight Wait() callers (settle window)
	cancel    context.CancelCauseFunc // nil cause = SIGKILL; ErrSignalINT/TERM = graceful
	// stdinWriter is the write end of the subprocess stdin pipe. Non-nil only
	// while the task is running; closed when the task ends. SendStdin() writes
	// here to inject input into the running process (e.g. answers to prompts).
	stdinWriter *io.PipeWriter

	// done is closed when the goroutine returns. Wait blocks on
	// it (or ctx) ; multiple waiters allowed.
	done chan struct{}

	// notifyWhen, when non-empty, starts a watcher goroutine that fires
	// exactly once (via notifyOnce) when the pattern appears in live output.
	notifyWhen string
	notifyOnce sync.Once
}

func (t *task) snapshot() meta.BackgroundStatus {
	s := meta.BackgroundStatus{
		TaskID:    t.id,
		Name:      t.name,
		StartedAt: t.startedAt,
	}
	if v := t.state.Load(); v != nil {
		s.State, _ = v.(string)
	}
	if v := t.result.Load(); v != nil {
		s.Result = v
	}
	if v := t.errMsg.Load(); v != nil {
		s.Error, _ = v.(string)
	}
	if t.live != nil {
		s.Log = t.live.tail()
	}
	return s
}

// Launch starts a new task. Returns the task id on success ;
// returns an error when the per-session cap is reached or the
// dispatcher isn't attached.
func (m *Manager) Launch(ctx context.Context, req meta.LaunchRequest) (string, error) {
	if m.dispatcher == nil {
		return "", errors.New("background: dispatcher not attached")
	}
	sessionID := req.SessionID
	if sessionID == "" {
		sessionID = "anon"
	}
	if req.Tool == "" {
		return "", errors.New("background: name required")
	}

	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	max := m.MaxTasksPerSession
	if max <= 0 {
		max = DefaultMaxTasks
	}
	if len(tbl.tasks) >= max {
		// Try a sweep first ; cheap and bounded.
		m.reapLocked(tbl)
		if len(tbl.tasks) >= max {
			tbl.mu.Unlock()
			return "", fmt.Errorf("background: per-session cap reached (%d)", max)
		}
	}
	id := uuid.NewString()
	// Inherit the launch ctx's VALUES — most importantly the session's workdir
	// (PathPolicy), so a backgrounded `node app.js` runs in the session
	// directory and not the daemon's cwd — but NOT its cancellation: the task
	// outlives the turn that launched it (fire-and-forget) and carries its own
	// cancel for the client stop endpoint.
	taskCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))
	t := &task{
		id:         id,
		name:       req.Tool,
		args:       req.Args,
		sessionID:  sessionID,
		appID:      req.AppID,
		userID:     req.UserID,
		agentID:    req.AgentID,
		startedAt:  m.now().Unix(),
		cancel:     cancel,
		live:       &liveLog{},
		done:       make(chan struct{}),
		notifyWhen: req.NotifyWhen,
	}
	t.state.Store("running")
	tbl.tasks[id] = t
	tbl.mu.Unlock()

	// Publish "running" so the client sees the task appear instantly.
	m.emit(t, "running", 0)

	go m.runTask(taskCtx, t)

	return id, nil
}

// runTask is the per-task goroutine body.
func (m *Manager) runTask(ctx context.Context, t *task) {
	defer close(t.done)
	// Start pattern watcher before the dispatch so it catches output from
	// the very first bytes written to the live log.
	if t.notifyWhen != "" {
		go m.watchPattern(t)
	}
	// Last-resort panic guard. The dispatcher chokepoint already recovers, but
	// a panic anywhere in this goroutine (a nil dispatcher, a future refactor)
	// must NOT crash the daemon, and must not leave the task pinned "running"
	// forever with no notification. If we recover and the normal terminal path
	// didn't run, mark the task errored + notify so the agent learns it died.
	defer func() {
		if r := recover(); r != nil {
			if s, _ := t.state.Load().(string); s == "running" {
				t.endedAt.Store(m.now().UnixNano())
				t.errMsg.Store(fmt.Sprintf("background task panicked: %v", r))
				t.state.Store("errored")
				m.enqueueNotification(t.sessionID, CompletionNotification{
					TaskID: t.id, ToolName: t.name, Status: "errored",
					Output: fmt.Sprintf("panic: %v", r),
				})
				if m.waker != nil {
					m.waker.WakeSession(t.appID, t.sessionID, t.userID)
				}
			}
		}
	}()
	// Create a stdin pipe so SendStdin() can inject input into the subprocess.
	// The read end travels via context to the bash module (detached.go uses it);
	// the write end is stored on the task for SendStdin() to write to.
	stdinR, stdinW := io.Pipe()
	t.stdinWriter = stdinW
	defer func() {
		// Close the write end when the task ends so the subprocess sees EOF.
		// This prevents it from hanging forever waiting for more stdin.
		_ = stdinW.Close()
		t.stdinWriter = nil
	}()

	// Mark the dispatch as asynchronous so a module that holds interactive
	// per-session state (the bash shell) runs this in an independent,
	// separately-cancellable process instead of blocking the session.
	dispatchCtx := tool.WithStdinPipe(tool.WithLiveSink(tool.WithBackground(ctx), t.live), stdinR)
	out := m.dispatcher.Dispatch(dispatchCtx, runtime.ToolInvocation{
		CallID:    t.id,
		Name:      t.name,
		Args:      t.args,
		AppID:     t.appID,
		AgentID:   t.agentID,
		UserID:    t.userID,
		SessionID: t.sessionID,
	})
	endNano := m.now().UnixNano()

	txt := ""
	for _, p := range out.Parts {
		txt += p.Text
	}

	state := "completed"
	reason := ""
	if out.Status == "errored" {
		state = "errored"
		reason = out.Error
	}
	if ctx.Err() != nil {
		// A cancel is the controlling outcome, but KEEP a concrete tool error
		// (EADDRINUSE, a build failure) as the reason if the tool reported one —
		// don't mask the real WHY behind a bland "context canceled".
		state = "cancelled"
		if reason == "" {
			reason = ctx.Err().Error()
		}
	}

	// Publish result / error / endedAt BEFORE the terminal state. A reader
	// (Status/List) observes `state` last, so once it sees a terminal state the
	// result and error are already visible — eliminating the "completed but
	// empty result" snapshot race.
	if txt != "" {
		t.result.Store(txt)
	}
	if reason != "" {
		t.errMsg.Store(reason)
	}
	t.endedAt.Store(endNano)
	t.state.Store(state)

	elapsedMs := (endNano - t.startedAt*int64(time.Second)) / int64(time.Millisecond)
	if elapsedMs < 0 {
		elapsedMs = 0
	}

	// Push the terminal lifecycle event so the live client view updates
	// instantly (running → completed/errored/cancelled).
	m.emit(t, state, elapsedMs)

	// Doc-conform : enqueue a completion notification for the
	// session's next turn. The runtime drains these on turn_start.
	// Carry the captured output (bounded tail) so a failed task tells the
	// agent WHY — an EADDRINUSE, a build error — instead of just "it failed".
	// Message() trims further at render ; this only bounds queue memory so a
	// chatty task can't pin megabytes in the pending slice.
	// Suppress the async notification + wake when a synchronous waiter (the
	// background_run settle window) is consuming this completion : it returns the
	// terminal result directly to the agent, so an async "[BACKGROUND TASK …]"
	// for the same task would be a confusing duplicate. The durable lifecycle
	// event (emit, above) still fires for the live client view. A task that
	// outlives the settle window has no waiter here, so it notifies normally.
	if t.waiters.Load() == 0 {
		const maxNotifyOutput = 8 << 10
		notifyOut := txt
		if len(notifyOut) > maxNotifyOutput {
			notifyOut = "…(truncated)…\n" + notifyOut[len(notifyOut)-maxNotifyOutput:]
		}
		m.enqueueNotification(t.sessionID, CompletionNotification{
			TaskID:    t.id,
			ToolName:  t.name,
			ElapsedMs: elapsedMs,
			Status:    state,
			Output:    notifyOut,
		})
		// Proactively wake the agent so it processes the completion now instead
		// of waiting for the next user message. The waker serializes turns per
		// session, so this can't collide with an in-flight turn — it coalesces.
		if m.waker != nil {
			m.waker.WakeSession(t.appID, t.sessionID, t.userID)
		}
	}
}

// emit publishes a background-task lifecycle event on the sink (if wired).
// Uses a background context so a cancelled task ctx can't suppress its own
// terminal event. Best-effort : a sink error is swallowed (the next-turn
// auto-notification is the durable agent-facing path).
func (m *Manager) emit(t *task, state string, elapsedMs int64) {
	if m.sink == nil {
		return
	}
	errMsg, _ := t.errMsg.Load().(string)
	if state != "errored" {
		errMsg = ""
	}
	_, _ = m.sink.AppendDurable(context.Background(), sessionstore.Event{
		Type:          sessionstore.EventBackgroundTask,
		SessionID:     t.sessionID,
		AppID:         t.appID,
		UserID:        t.userID,
		CorrelationID: t.id,
		Background: &sessionstore.BackgroundTaskPayload{
			TaskID:        t.id,
			Tool:          t.name,
			Label:         taskLabel(t),
			State:         state,
			Error:         errMsg,
			ElapsedMs:     elapsedMs,
			StartedAtUnix: t.startedAt,
		},
	})
}

func taskLabel(t *task) string {
	action := t.name
	if idx := strings.LastIndex(action, "."); idx >= 0 {
		action = action[idx+1:]
	}
	primaryKeys := []string{"command", "query", "url", "path", "file_path", "filePath", "pattern", "task", "prompt"}
	for _, k := range primaryKeys {
		if v, ok := t.args[k].(string); ok && v != "" {
			short := strings.ReplaceAll(v, "\n", " ")
			if len(short) > 80 {
				short = short[:79] + "…"
			}
			return action + " › " + short
		}
	}
	for _, v := range t.args {
		if s, ok := v.(string); ok && s != "" {
			short := strings.ReplaceAll(s, "\n", " ")
			if len(short) > 80 {
				short = short[:79] + "…"
			}
			return action + " › " + short
		}
	}
	return action
}

// enqueueNotification appends a CompletionNotification to the
// per-session queue. Safe under concurrent task completion.
// maxPendingNotifications bounds the per-session pending-notification queue. A
// session that completes many tasks but never takes another turn (the user
// left, or no waker fired) would otherwise grow this slice forever. We keep the
// most recent N (the agent cares about recent completions; Status/List still
// hold the full task picture). Prevents an unbounded memory leak.
const maxPendingNotifications = 256

func (m *Manager) enqueueNotification(sessionID string, n CompletionNotification) {
	m.notMu.Lock()
	defer m.notMu.Unlock()
	if m.notifications == nil {
		m.notifications = map[string][]CompletionNotification{}
	}
	q := append(m.notifications[sessionID], n)
	if len(q) > maxPendingNotifications {
		q = q[len(q)-maxPendingNotifications:]
	}
	m.notifications[sessionID] = q
}

// DrainNotifications returns and clears the queue of completed
// task notifications for one session. The runtime calls this at
// turn_start and injects each notification as a system message
// per docs-site/language/04c-primitives.md "Auto-notification".
//
// Returns an empty slice when nothing's pending. Safe to call
// concurrently with new completions.
func (m *Manager) DrainNotifications(sessionID string) []CompletionNotification {
	m.notMu.Lock()
	defer m.notMu.Unlock()
	if m.notifications == nil {
		return nil
	}
	q := m.notifications[sessionID]
	if len(q) == 0 {
		return nil
	}
	delete(m.notifications, sessionID)
	return q
}

// DrainNotificationsPeek returns a copy of the pending notifications
// without consuming them. Used by tests / observability ; the
// runtime hot path always uses DrainNotifications.
func (m *Manager) DrainNotificationsPeek(sessionID string) []CompletionNotification {
	m.notMu.Lock()
	defer m.notMu.Unlock()
	q := m.notifications[sessionID]
	out := make([]CompletionNotification, len(q))
	copy(out, q)
	return out
}

// Status returns a snapshot of one task. Returns error when the
// task id is unknown for this session.
func (m *Manager) Status(_ context.Context, sessionID, taskID string) (meta.BackgroundStatus, error) {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	t, ok := tbl.tasks[taskID]
	if !ok {
		return meta.BackgroundStatus{}, fmt.Errorf("background: task %q not found", taskID)
	}
	return t.snapshot(), nil
}

// Wait blocks until the task finishes or the timeout expires. A
// zero timeout means "no timeout".
func (m *Manager) Wait(ctx context.Context, sessionID, taskID string, timeoutSecs float64) (meta.BackgroundStatus, error) {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	t, ok := tbl.tasks[taskID]
	tbl.mu.Unlock()
	if !ok {
		return meta.BackgroundStatus{}, fmt.Errorf("background: task %q not found", taskID)
	}

	if timeoutSecs > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(timeoutSecs*float64(time.Second)))
		defer cancel()
	}
	// Register as a synchronous waiter. While at least one waiter is in flight,
	// runTask suppresses the async completion notification — the waiter (the
	// settle window) returns the terminal result directly, so the agent must not
	// also be told about the same completion a second time. The ordering is
	// race-free: runTask enqueues the notification BEFORE it closes t.done, and
	// this waiter stays registered until t.done closes (or the timeout fires), so
	// the waiter count is non-zero exactly when runTask makes the decision.
	t.waiters.Add(1)
	defer t.waiters.Add(-1)
	select {
	case <-t.done:
		return t.snapshot(), nil
	case <-ctx.Done():
		// Return the current snapshot (likely "running") plus the
		// ctx err so the LLM sees the timeout.
		s := t.snapshot()
		return s, ctx.Err()
	}
}

// Cancel signals the task to stop immediately (SIGKILL). The task's snapshot
// may still report "running" briefly until the goroutine observes the cancel.
func (m *Manager) Cancel(_ context.Context, sessionID, taskID string) error {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	t, ok := tbl.tasks[taskID]
	if !ok {
		return fmt.Errorf("background: task %q not found", taskID)
	}
	t.cancel(nil) // nil cause → SIGKILL in bash module
	return nil
}

// SendStdin writes input to the running task's subprocess stdin. The data
// is forwarded through the io.Pipe created at launch time and consumed by
// the bash module's subprocess (detached.go). This lets the agent answer
// interactive prompts (passwords, confirmations, REPL input) mid-execution.
// Returns an error when the task is not found, already finished, or the pipe
// was not set up (non-bash tasks).
func (m *Manager) SendStdin(_ context.Context, sessionID, taskID, input string) error {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	t, ok := tbl.tasks[taskID]
	tbl.mu.Unlock()
	if !ok {
		return fmt.Errorf("background: task %q not found", taskID)
	}
	w := t.stdinWriter
	if w == nil {
		return fmt.Errorf("background: task %q has no stdin pipe (task may have finished or not support stdin injection)", taskID)
	}
	_, err := io.WriteString(w, input)
	return err
}

// Signal sends a graceful OS signal to the task's running process.
// sig must be "SIGINT" or "SIGTERM". The bash module reads context.Cause()
// and uses the appropriate syscall instead of the default SIGKILL.
func (m *Manager) Signal(_ context.Context, sessionID, taskID, sig string) error {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	t, ok := tbl.tasks[taskID]
	if !ok {
		return fmt.Errorf("background: task %q not found", taskID)
	}
	switch strings.ToUpper(sig) {
	case "SIGINT", "INT", "2":
		t.cancel(ErrSignalINT)
	case "SIGTERM", "TERM", "15":
		t.cancel(ErrSignalTERM)
	default:
		return fmt.Errorf("background: unsupported signal %q (use SIGINT or SIGTERM)", sig)
	}
	return nil
}

// CancelAllForSession stops every RUNNING task in a session and returns how
// many were signalled. Part of the total session abort : a user "stop" must
// halt background work too, not just the foreground turn. Cancels are gathered
// under the table lock and fired outside it (a cancel callback must never
// deadlock against Launch/Status). Each task's goroutine observes the cancel,
// marks itself "cancelled", and emits its durable terminal event.
func (m *Manager) CancelAllForSession(sessionID string) int {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	cancels := make([]context.CancelCauseFunc, 0, len(tbl.tasks))
	for _, t := range tbl.tasks {
		if state, _ := t.state.Load().(string); state == "running" {
			cancels = append(cancels, t.cancel)
		}
	}
	tbl.mu.Unlock()
	for _, c := range cancels {
		c(nil) // nil cause = SIGKILL (session abort)
	}
	return len(cancels)
}

// List returns snapshots of every task in the session. Reaps
// completed tasks older than RetainCompleted as a side-effect to
// keep the table bounded.
func (m *Manager) List(_ context.Context, sessionID string) ([]meta.BackgroundStatus, error) {
	tbl := m.tableFor(sessionID)
	tbl.mu.Lock()
	defer tbl.mu.Unlock()
	m.reapLocked(tbl)
	out := make([]meta.BackgroundStatus, 0, len(tbl.tasks))
	for _, t := range tbl.tasks {
		out = append(out, t.snapshot())
	}
	return out, nil
}

// reapLocked deletes completed/cancelled/errored tasks whose
// endedAt + RetainCompleted is in the past. Caller MUST hold
// tbl.mu.
func (m *Manager) reapLocked(tbl *sessionTable) {
	retain := m.RetainCompleted
	if retain <= 0 {
		retain = DefaultRetain
	}
	cutoff := m.now().Add(-retain).UnixNano()
	for id, t := range tbl.tasks {
		state, _ := t.state.Load().(string)
		if state == "running" {
			continue
		}
		ended := t.endedAt.Load()
		if ended == 0 || ended < cutoff {
			delete(tbl.tasks, id)
		}
	}
}

// tableFor returns (lazily creating) the per-session task table.
func (m *Manager) tableFor(sessionID string) *sessionTable {
	if v, ok := m.sessions.Load(sessionID); ok {
		return v.(*sessionTable)
	}
	fresh := &sessionTable{tasks: make(map[string]*task)}
	if actual, loaded := m.sessions.LoadOrStore(sessionID, fresh); loaded {
		return actual.(*sessionTable)
	}
	return fresh
}

func (m *Manager) now() time.Time {
	if m.nowFn != nil {
		return m.nowFn()
	}
	return time.Now()
}

// Compile-time guard : *Manager satisfies the meta.BackgroundManager interface.
var _ meta.BackgroundManager = (*Manager)(nil)
