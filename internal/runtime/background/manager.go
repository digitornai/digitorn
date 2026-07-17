package background

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
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

var (
	ErrSignalINT  = errors.New("background: SIGINT requested")
	ErrSignalTERM = errors.New("background: SIGTERM requested")
)

type EventSink interface {
	AppendDurable(ctx context.Context, ev sessionstore.Event) (uint64, error)
}

type Waker interface {
	WakeSession(appID, sessionID, userID string)
}

type Manager struct {
	dispatcher runtime.ToolDispatcher

	sink EventSink

	waker Waker

	WorkspaceTouched func(sessionID string)

	DevServerDetected func(sessionID, url string)

	MaxTasksPerSession int

	RetainCompleted time.Duration

	nowFn func() time.Time

	sessions sync.Map

	notMu         sync.Mutex
	notifications map[string][]CompletionNotification
}

type CompletionNotification struct {
	TaskID    string
	ToolName  string
	ElapsedMs int64
	Status    string
	Output    string
}

func (n CompletionNotification) Message() string {
	prefix := "[BACKGROUND TASK COMPLETED]"
	switch n.Status {
	case "errored":
		prefix = "[BACKGROUND TASK FAILED]"
	case "cancelled":
		prefix = "[BACKGROUND TASK CANCELLED]"
	case "pattern_matched":
		prefix = "[BACKGROUND TASK READY]"
	}
	msg := fmt.Sprintf("%s task_id=%s tool=%s elapsed=%.1fs",
		prefix, n.TaskID, n.ToolName, float64(n.ElapsedMs)/1000)
	if out := strings.TrimSpace(n.Output); out != "" && n.Status != "completed" {
		const max = 2000
		if len(out) > max {
			out = "…(truncated)…\n" + out[len(out)-max:]
		}
		msg += "\noutput:\n" + out
	}
	return msg
}

func (m *Manager) watchPattern(t *task) {
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
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

var localServerRe = regexp.MustCompile(`https?://(localhost|127\.0\.0\.1|0\.0\.0\.0):(\d{2,5})(/[^\s"'` + "`" + `\x1b]*)?`)

func detectLocalServerURL(s string) string {
	m := localServerRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	host := m[1]
	if host == "0.0.0.0" {
		host = "localhost"
	}
	path := m[3]
	if path == "" {
		path = "/"
	}
	return "http://" + host + ":" + m[2] + path
}

func (m *Manager) watchDevServer(t *task) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.After(2 * time.Minute)
	fire := func() bool {
		url := detectLocalServerURL(t.live.tail())
		if url == "" {
			return false
		}
		t.devServerOnce.Do(func() { m.DevServerDetected(t.sessionID, url) })
		return true
	}
	for {
		select {
		case <-t.done:
			fire()
			return
		case <-ticker.C:
			if fire() {
				return
			}
		case <-deadline:
			return
		}
	}
}

const (
	DefaultMaxTasks = 100
	DefaultRetain   = 30 * time.Minute
)

func New() *Manager {
	return &Manager{
		nowFn:         time.Now,
		notifications: map[string][]CompletionNotification{},
	}
}

func (m *Manager) AttachDispatcher(d runtime.ToolDispatcher) {
	m.dispatcher = d
}

func (m *Manager) AttachSink(s EventSink) {
	m.sink = s
}

func (m *Manager) AttachWaker(w Waker) {
	m.waker = w
}

type sessionTable struct {
	mu    sync.Mutex
	tasks map[string]*task
}

type task struct {
	id        string
	name      string
	args      map[string]any
	sessionID string
	appID     string
	userID    string
	agentID   string
	state     atomic.Value
	startedAt int64
	endedAt   atomic.Int64
	result    atomic.Value
	errMsg    atomic.Value
	live      *liveLog
	waiters   atomic.Int32
	cancel    context.CancelCauseFunc
	stdinWriter *io.PipeWriter

	done chan struct{}

	notifyWhen string
	notifyOnce sync.Once
	devServerOnce sync.Once
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
		m.reapLocked(tbl)
		if len(tbl.tasks) >= max {
			tbl.mu.Unlock()
			return "", fmt.Errorf("background: per-session cap reached (%d)", max)
		}
	}
	id := uuid.NewString()
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

	m.emit(t, "running", 0)

	go m.runTask(taskCtx, t)

	return id, nil
}

func (m *Manager) runTask(ctx context.Context, t *task) {
	defer close(t.done)
	if t.notifyWhen != "" {
		go m.watchPattern(t)
	}
	if m.DevServerDetected != nil {
		go m.watchDevServer(t)
	}
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

	if m.WorkspaceTouched != nil && t.sessionID != "" {
		m.WorkspaceTouched(t.sessionID)
	}

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
