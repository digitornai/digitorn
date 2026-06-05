package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/mbathepaul/digitorn-cli/internal/client"
	"github.com/mbathepaul/digitorn-cli/internal/theme"
)

// ChatScreen is the active screen when Screen == ScreenChat. It owns
// the input + viewport + sidebar, the session ID, and the Socket.IO
// stream that pushes daemon events in real time. REST is used for
// the initial warm-start history fetch and for sending user messages
// (POST /messages) ; everything else flows through realtime.
type ChatScreen struct {
	client          *client.Client
	appID           string
	appName         string
	appVersion      string
	sessionID       string
	resumeSessionID string
	workdir         string // resolved workdir reported by the daemon (sidebar)
	reqWorkdir      string // requested workdir to send on session creation (launch dir)
	userID          string
	theme           *theme.Theme

	input    *Input
	messages *Messages
	sidebar  *Sidebar

	rt       *client.Realtime
	connStop chan struct{}
	envCh    chan client.Envelope

	width  int
	height int

	// pendingTurn flips on turn_started (or optimistically on send) and back
	// off on turn_ended. Drives the "[running]" phase badge.
	pendingTurn  bool
	currentPhase string

	// sendQueue holds messages typed while a turn is in flight. We post them
	// one at a time as each turn ends rather than firing them all at the daemon
	// (whose single-flight would coalesce a rapid burst into one reply). FIFO.
	sendQueue []string

	// streamBuf accumulates assistant_delta tokens for the in-progress
	// reply, rendered live as a transient bubble. Reset at each
	// message_started, cleared when the final assistant_message lands.
	streamBuf string

	// streamingTools tracks tool calls the model is STILL emitting (status
	// "streaming"), keyed by call_id, with insertion order. They render in the
	// live bottom area (below the streaming text), NEVER as seq'd timeline
	// chips — a streaming event's seq is lower than the assistant_message that
	// announces it, so a timeline chip would sort ahead of the intent. The real
	// ordered chip is created from the durable pending/result event.
	streamingTools   map[string]*streamingTool
	streamingToolIDs []string

	// thinkBuf accumulates the agent's live thinking (reasoning) trace during
	// generation, rendered dimmed in the live area. Cleared when the message
	// commits ; the consolidated reasoning then rides the message itself.
	thinkBuf string

	// turnChars counts characters streamed this turn (across rounds) so the
	// working indicator can show a live ~token counter (≈ chars/4 ; we have no
	// tokenizer client-side). Reset at turn_started.
	turnChars int

	// turnTokens is the running OUTPUT-token count for the current turn, summed
	// ACROSS rounds. The daemon's LiveOutputTokens (CTX-7.5) is per assistant
	// MESSAGE and restarts each LLM round, so a multi-round turn (tool calls)
	// would otherwise see the counter reset mid-turn. We keep it monotonic by
	// banking each finished round's tokens into turnTokensBase and adding the
	// current message's live estimate on top. Reset at turn_started.
	turnTokens      int
	turnTokensExact bool
	// turnTokensBase is the output tokens of rounds ALREADY committed this turn
	// (rolled in on each assistant_message). turnRounds counts those commits, so
	// token_usage (which carries only the LAST round's exact tokens, not the turn
	// total) snaps to the exact value only for a single-round turn.
	turnTokensBase int
	turnRounds     int

	// ctxUsed / ctxWindow are the EXACT context occupancy and the model's
	// context window, both from the daemon's context_tokens event (CTX-7).
	// Shown in the composer footer as "ctx used/window" — it climbs as the
	// conversation grows and drops when the daemon compacts. ctxWindow 0 until
	// the first recount lands (then the footer switches from the char counter).
	ctxUsed   int
	ctxWindow int

	// compacting is true between a context_compacting and its context_compacted
	// — shown live next to the spinner above the input. The end-of-compaction
	// summary is injected into the chat thread (at the event's seq), not held
	// here. Reset on turn_started.
	compacting bool

	connState client.ConnState
	lastErr   error
	// connRetrying is set while we're retrying an initial connect that failed
	// (socket.io auto-reconnect only covers post-connect drops). Gates the
	// retry toast so it shows once, not on every attempt.
	connRetrying bool

	// loadingSession is set while a just-opened session's backlog replays in, so
	// the transcript is hidden behind a spinner and revealed whole on replay_done
	// instead of flickering message-by-message. Cleared on replay_done, a connect
	// failure, or a safety timeout.
	loadingSession bool

	// lastSent is the most recent user message text we posted, so Ctrl+R can
	// re-send it (retry a failed turn) without retyping.
	lastSent string

	// Transcript search (Ctrl+F). searching gates the keystroke routing : while
	// on, typed runes build searchQuery, ↑/↓ walk searchMatches (message indices),
	// esc closes. searchPos is the active match. The current match is shown via
	// the messages widget's own selection highlight.
	searching     bool
	searchQuery   string
	searchMatches []int
	searchPos     int

	// Picker overlay state. picker != nil = overlay is active, keys
	// route to it, the chat body is dimmed and unfocused.
	picker     *Picker
	pickerKind string // "sessions" | "apps"

	// Approval modal. approval != nil = a tool call is gated behind
	// the `approve` policy and the turn is suspended ; keys route to
	// the modal until the user answers (or the daemon resolves it).
	approval *ApprovalPrompt

	// askForm is the interactive ask_user prompt (kind=="question") :
	// free text / content review / single- or multi-choice / structured
	// form. Distinct from approval (a fixed y/a/n tool gate) ; keys route
	// here while it's open and the assembled answer goes back as the
	// approval reason.
	askForm *AskForm

	// Slash-command autocomplete : a small popup that appears above
	// the input as soon as the user types "/". Filters live as the
	// user types more characters.
	complActive   bool
	complCursor   int
	complFiltered []slashCmdSpec

	// Transient top-right notifications + the waiting shimmer. Both are
	// driven by a single animation tick that only runs while there is
	// something to animate (toasts present, or a turn awaiting its first
	// token) — no idle redraw loop.
	toasts       []toastItem
	ticking      bool
	shimmerFrame int

	// help overlay : when true, the help card is shown and any key closes it.
	help bool

	// drillParent is the session to return to when viewing a sub-agent's
	// isolated child session (drill-in). Empty = not drilled in.
	drillParent string

	// model is the entry agent's configured model, fetched from the app
	// manifest, shown in the status bar.
	model string

	// agentCalls marks which tool call_ids are sub-agent delegations, so the
	// matching tool_result is recognised by call_id even when the call was
	// wrapped in execute_tool (whose result payload carries no arguments to
	// unwrap). Without this the result renders as a stray execute_tool chip
	// and the agent chip stays stuck on "running".
	// seenSeq is the highest event seq processed per session id, for dedup : on
	// reconnect, join_session (live) and replay-since overlap, re-delivering
	// events the client already handled. Keyed by session id so root and each
	// sub-session (independent seq spaces) dedup separately. Reset on switch.
	seenSeq map[string]uint64

	agentCalls map[string]bool
	// agentRunCall maps a sub-agent run_id → the tool call_id of its chip,
	// so an async agent_result (which carries run_id, not call_id) can
	// finalise the right chip.
	agentRunCall map[string]string
	// Sub-agent drill-in : the child session id of a delegation, keyed by
	// the agent tool's call_id (what the chip carries) ; agentRunChild is
	// the spawn-time run_id → child map used to resolve the call↔run link
	// when the tool result lands.
	agentChild    map[string]string
	agentRunChild map[string]string
	// agentRunKind maps a sub-agent run_id → its logical agent id (kind),
	// captured at agent_spawn, so the live activity fanned out from the
	// sub-session can be attributed to a named sub-agent in the trace.
	agentRunKind map[string]string
	// agentToolCount maps a sub-agent run_id → how many tools it has run so far,
	// counted live from its fanned tool_call events. lastAgentCallByKind maps an
	// agent kind → the call_id of its most recent delegation chip, so a spawn can
	// bind its run_id to that chip and the count lands on the right chip live.
	agentToolCount      map[string]int
	lastAgentCallByKind map[string]string
	// parallelArgs maps a run_parallel call_id → the per-sub-task argument
	// hints (file/command), captured from the tool_call so the expanded group
	// can show what each parallel sub-tool operated on (the result only carries
	// names + status, not the inputs).
	parallelArgs map[string][]string
	// todos is the agent's task list (memory.task_create / task_update), kept
	// in arrival order. Rendered as a checklist inline in the chat AND in the
	// right sidebar — never as raw tool chips. Main agent only.
	todos []todoItem
	// subActivity holds live per-sub-agent activity for the right panel : each
	// active sub-agent keeps a PINNED header plus a bounded window of its most
	// recent tools (older ones scroll off, so the list never grows without
	// bound), and the whole group is dropped when the sub-agent finishes. Keyed
	// by run_id, in spawn order.
	subActivity []*subAgentActivity
	// pendingApproval is the label of the approval/question awaiting the user
	// RIGHT NOW (empty once resolved), shown as a single line in the activity
	// panel — never a growing history of granted/denied rows.
	pendingApproval string
	// approvalCallID is the tool_call id of the chip currently gated behind an
	// approval, so we can flip it to "awaiting" while the modal is up and back to
	// "running" once the user grants it (instead of a misleading spinner).
	approvalCallID string

	// modes are the app's declared composer modes (runtime.modes), in YAML order ;
	// modeIdx is the active one, cycled with Shift+Tab and sent with each message.
	// Empty when the app declares no modes (the switcher then stays hidden).
	modes   []client.Mode
	modeIdx int
}

// subAgentActivity is one agent's slice of the activity panel : the most recent
// FINISHED tools (bounded), the tools still RUNNING (shown individually up to a
// cap, the overflow collapsed into "⠋ … xN" so a parallel burst can't blow up
// the block), and a count of finished rows that scrolled off.
type subAgentActivity struct {
	runID    string
	kind     string
	finished []TimelineEntry // recent ✓/✗ rows, bounded — feeds the "· N tools" count only
	running  []TimelineEntry // in-flight rows (Label has no glyph), in start order
	settling []TimelineEntry // just-finished ghosts fading out (rendered, then pruned)
	hidden   int             // finished rows scrolled off the top
}

// subActivityFinishedMax is how many recent FINISHED rows a group shows (older
// ones scroll into a "…"). subActivityRunMax is how many RUNNING tools are
// listed individually before the overflow collapses into "⠋ … xN" — so some
// current work is always visible without a parallel burst filling the rail.
const (
	subActivityFinishedMax = 2
	subActivityRunMax      = 4
	// Activity-rail transitions, counted in animation ticks (animInterval ≈
	// 120 ms each) : a new tool is faint for activityFadeTicks before going
	// solid, and a finished tool lingers as a fading ✓/✗ ghost for
	// activitySettleTicks before it's dropped — so rows ease in and out.
	activityFadeTicks   = 1
	activitySettleTicks = 3
)

// animTickMsg drives toast expiry + the shimmer sweep. Re-scheduled by
// the handler only while animation is still needed.
type animTickMsg struct{}

const animInterval = 120 * time.Millisecond

// ensureTick starts the animation loop if something needs animating and
// it isn't already running. Returned from any Update branch that may have
// created a toast or started a turn.
func (s *ChatScreen) ensureTick() tea.Cmd {
	if s.ticking || !s.animating() {
		return nil
	}
	s.ticking = true
	return s.tickCmd()
}

// animating reports whether anything needs periodic repaint : toasts to
// expire, the waiting shimmer, or an in-progress chip whose live duration
// is ticking. A pending turn ALWAYS keeps the tick alive — otherwise a quiet
// gap mid-turn (the model thinking between rounds, a long tool call emitting
// no events) lets animating() fall to false, the tick stops, and the working
// indicator freezes even though the turn is still running.
func (s *ChatScreen) animating() bool {
	return s.pendingTurn || len(s.toasts) > 0 || s.shimmering() || s.messages.HasRunning() ||
		len(s.streamingToolIDs) > 0 || s.loadingSession || (s.picker != nil && s.picker.Loading())
}

func (s *ChatScreen) tickCmd() tea.Cmd {
	return tea.Tick(animInterval, func(time.Time) tea.Msg { return animTickMsg{} })
}

// slashCmdSpec describes a command available via autocomplete.
type slashCmdSpec struct {
	Name string
	Desc string
}

// allSlashCommands is the static catalog the autocomplete pulls from.
var allSlashCommands = []slashCmdSpec{
	{Name: "sessions", Desc: "switch to another session of this app"},
	{Name: "apps", Desc: "switch to another installed app"},
	{Name: "new", Desc: "start a fresh session"},
	{Name: "theme", Desc: "switch color theme (live)"},
	{Name: "help", Desc: "show all slash commands"},
	{Name: "quit", Desc: "exit the TUI"},
}

// TimelineEntry is one tool row in a sub-agent's activity group (running,
// settling, or counted-as-finished).
type TimelineEntry struct {
	Type  string
	Label string
	// CallID keys a tool row so its result UPDATES the call row in place
	// (⚙ → ✓ / ✗) instead of appending a second line — the activity panel is a
	// live view of operations, not an append-only log. Empty for non-tool rows.
	CallID string

	// Animation state for the activity rail, advanced once per tick :
	//   age    — ticks since the row appeared ; while 0 it renders faint so a
	//            new tool fades in instead of popping.
	//   settle — for a "settling" ghost (a just-finished tool kept briefly so it
	//            fades out instead of vanishing) : ticks left before removal.
	//   ok     — the settling ghost's result, picks ✓ (true) vs ✗ (false).
	age    int
	settle int
	ok     bool
}

func NewChatScreen(c *client.Client, appID string, m *Model) *ChatScreen {
	uid := ""
	if m.creds != nil {
		uid = client.DefaultUserID(m.creds)
	} else {
		uid = client.DefaultUserID(nil)
	}
	return &ChatScreen{
		client:    c,
		appID:     appID,
		appName:   m.statusBar.AppName,
		theme:     m.theme,
		userID:    uid,
		input:     NewInput(m.theme),
		messages:  NewMessages(m.theme),
		sidebar:   NewSidebar(m.theme),
		envCh:     make(chan client.Envelope, 1024),
		connState: client.ConnStateConnecting,
	}
}

// ---- messages (tea.Msg types specific to chat) -------------------

type sessionCreatedMsg struct {
	sessionID string
	workdir   string
	workspace string
	// resume marks a session reopened at launch (--resume) rather than freshly
	// created : it has a backlog to replay, so the open spinner applies.
	resume bool
	err    error
}

type messagePostedMsg struct {
	err error
}

type realtimeReadyMsg struct {
	rt   *client.Realtime
	stop chan struct{}
	err  error
}

type envelopeMsg struct {
	env client.Envelope
}

type approvalResolvedMsg struct {
	err error
}

type connStateMsg struct {
	state client.ConnState
	err   error
}

// sessionLoadTimeoutMsg fires after the session-open spinner has been up too
// long ; it reveals the transcript even if replay_done never arrived. Tagged
// with the session it was armed for so a stale timer is ignored.
type sessionLoadTimeoutMsg struct{ sessionID string }

// connectRetryMsg fires after a delay to re-attempt an initial connect that
// failed (the only reconnect path socket.io's own retry doesn't cover).
type connectRetryMsg struct{}

type abortResultMsg struct {
	err error
}

type modelLoadedMsg struct {
	model string
}

type modesLoadedMsg struct {
	modes  []client.Mode
	active string // the session's sticky mode id, if known (else "")
}

// FetchModel pulls the entry agent's model from the app manifest. Fired
// at startup and on every /apps switch. Failures are silent — the model
// segment simply stays hidden.
func (s *ChatScreen) FetchModel() tea.Cmd {
	c := s.client
	appID := s.appID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		model, err := c.AppModel(ctx, appID)
		if err != nil {
			client.Debugf("chat: AppModel failed: %v", err)
			return modelLoadedMsg{}
		}
		return modelLoadedMsg{model: model}
	}
}

// appInfoMsg carries the app's display name + version for the sidebar footer.
type appInfoMsg struct{ name, version string }

// FetchAppInfo pulls the app's name + version so the rail footer can show
// "<name> v<version>". Fired at startup and on /apps switch ; silent on error.
func (s *ChatScreen) FetchAppInfo() tea.Cmd {
	c := s.client
	appID := s.appID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		app, err := c.GetApp(ctx, appID)
		if err != nil {
			return appInfoMsg{}
		}
		return appInfoMsg{name: app.Name, version: app.Version}
	}
}

// FetchModes pulls the app's declared composer modes (runtime.modes) and, when
// a session is already open, that session's sticky active mode — so the switcher
// starts on the mode the session last used. Fired at startup and on /apps switch.
// Failures are silent : no modes simply hides the switcher.
func (s *ChatScreen) FetchModes() tea.Cmd {
	c := s.client
	appID := s.appID
	sid := s.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		modes, err := c.AppModes(ctx, appID)
		if err != nil {
			client.Debugf("chat: AppModes failed: %v", err)
			return modesLoadedMsg{}
		}
		active := ""
		if sid != "" {
			active, _ = c.SessionActiveMode(ctx, appID, sid)
		}
		return modesLoadedMsg{modes: modes, active: active}
	}
}

// applyModes installs the fetched mode list and selects the active one (the
// session's sticky mode if known, else the first declared = the app default),
// then mirrors the label into the composer footer.
func (s *ChatScreen) applyModes(modes []client.Mode, active string) {
	s.modes = modes
	s.modeIdx = 0
	if active != "" {
		for i, md := range modes {
			if md.ID == active {
				s.modeIdx = i
				break
			}
		}
	}
	s.syncModeFooter()
}

// cycleMode advances to the next declared mode (Shift+Tab), wrapping around.
func (s *ChatScreen) cycleMode() {
	if len(s.modes) == 0 {
		return
	}
	s.modeIdx = (s.modeIdx + 1) % len(s.modes)
	s.syncModeFooter()
}

// activeModeID is the id sent with each message (empty when no modes declared,
// which lets the daemon fall back to the session's sticky mode / app default).
func (s *ChatScreen) activeModeID() string {
	if s.modeIdx < 0 || s.modeIdx >= len(s.modes) {
		return ""
	}
	return s.modes[s.modeIdx].ID
}

// syncModeFooter pushes the active mode's label (with its icon) into the input
// footer so the current mode is always visible next to the composer.
func (s *ChatScreen) syncModeFooter() {
	if s.modeIdx < 0 || s.modeIdx >= len(s.modes) {
		s.input.SetMode("")
		return
	}
	md := s.modes[s.modeIdx]
	label := md.Label
	if md.Icon != "" {
		label = md.Icon + " " + label
	}
	s.input.SetMode(label)
}

// ---- bootstrap -----------------------------------------------------

// Bootstrap creates a fresh session (or resumes Options.ResumeSessionID) and
// then wires the realtime stream : connectRealtime → JoinSession → replay
// since=0, so the message history flows in via Socket.IO. Creating the session
// up front keeps the connection state honest (the socket is joined from launch)
// and avoids the new-session loading/connection edge cases of deferring it.
func (s *ChatScreen) Bootstrap() tea.Cmd {
	c := s.client
	appID := s.appID
	resume := s.resumeSessionID
	return func() tea.Msg {
		if resume != "" {
			return sessionCreatedMsg{sessionID: resume, resume: true}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.CreateSession(ctx, appID, client.CreateSessionRequest{
			Workdir: s.reqWorkdir,
		})
		if err != nil {
			return sessionCreatedMsg{err: err}
		}
		return sessionCreatedMsg{sessionID: resp.SessionID, workdir: resp.Workdir, workspace: resp.Workspace}
	}
}

func (s *ChatScreen) connectRealtime() tea.Cmd {
	return func() tea.Msg {
		rt, err := client.NewRealtime(client.RealtimeOptions{
			BaseURL: s.client.BaseURL(),
			Token:   s.client.BearerToken(),
			UserID:  s.userID,
		})
		if err != nil {
			return realtimeReadyMsg{err: err}
		}
		ch := s.envCh
		rt.OnEnvelope(func(env client.Envelope) {
			select {
			case ch <- env:
			default:
				// Buffer full. NEVER sacrifice a DURABLE event (a message, a tool
				// result, a final tool_call) for a high-frequency transient hint —
				// dropping an assistant_message here is exactly what made the
				// agent's intermediate messages vanish under a streaming flood.
				// Only transient hints are droppable : if THIS event is a hint,
				// drop it and leave the buffer (with its durable events) intact ;
				// otherwise make room and push the durable event through.
				if isTransientEnvelope(env) {
					return
				}
				select {
				case <-ch:
				default:
				}
				select {
				case ch <- env:
				default:
				}
			}
		})
		stateCh := make(chan struct {
			state client.ConnState
			err   error
		}, 16)
		rt.OnState(func(st client.ConnState, e error) {
			select {
			case stateCh <- struct {
				state client.ConnState
				err   error
			}{st, e}:
			default:
			}
		})

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		client.Debugf("chat: rt.Connect starting...")
		if err := rt.Connect(ctx); err != nil {
			client.Debugf("chat: rt.Connect failed: %v", err)
			_ = rt.Close() // don't leak a socket that may keep retrying in the background
			return realtimeReadyMsg{err: err}
		}
		client.Debugf("chat: rt.Connect ok ; joining session=%s", s.sessionID)
		if err := rt.JoinSession(s.appID, s.sessionID); err != nil {
			client.Debugf("chat: rt.JoinSession failed: %v", err)
			_ = rt.Close()
			return realtimeReadyMsg{err: err}
		}
		// Only now start forwarding state changes: the consumer's lifetime is
		// bound to `stop`, so the goroutine can't outlive the connection and
		// leak. The OnState callback's sends are non-blocking, so the buffered
		// stateCh safely absorbs anything emitted during Connect/JoinSession.
		stop := make(chan struct{})
		go s.pumpStates(stateCh, stop)

		// Hand the connected client back to Update; assigning s.rt here would
		// race the Bubble Tea loop, which owns ChatScreen state.
		return realtimeReadyMsg{rt: rt, stop: stop}
	}
}

// scheduleConnectRetry re-attempts the realtime connect after a short delay.
// Used only for the initial-connect-failed case (post-connect drops are handled
// by socket.io's own backoff). Fixed 2s — fast enough to feel responsive when
// the daemon comes up, gentle enough not to hammer it.
func (s *ChatScreen) scheduleConnectRetry() tea.Cmd {
	return tea.Tick(2*time.Second, func(time.Time) tea.Msg { return connectRetryMsg{} })
}

// scheduleSessionLoadTimeout is a safety net : if replay_done never arrives (a
// dropped socket mid-replay, a daemon that didn't emit it), reveal whatever
// streamed in rather than spinning forever. Tagged with the session it covers
// so a stale timer from a session we already left can't blank the new one.
func (s *ChatScreen) scheduleSessionLoadTimeout() tea.Cmd {
	sid := s.sessionID
	return tea.Tick(8*time.Second, func(time.Time) tea.Msg { return sessionLoadTimeoutMsg{sessionID: sid} })
}

// pumpStates forwards Realtime state changes onto the bubbletea program via
// the env channel. We piggy-back on envCh by tagging state changes inside the
// chat screen's Update — actually simpler : we expose a separate goroutine
// path. The bubbletea idiom is to translate via tea.Program.Send, but the
// screen doesn't own the program ; so we re-emit state changes as a special
// envelope on envCh with Type="__conn__".
func (s *ChatScreen) pumpStates(in <-chan struct {
	state client.ConnState
	err   error
}, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case ev := <-in:
			payload := map[string]any{"state": int(ev.state)}
			if ev.err != nil {
				payload["err"] = ev.err.Error()
			}
			select {
			case s.envCh <- client.Envelope{Type: "__conn__", Payload: payload}:
			case <-stop:
				return
			}
		}
	}
}

// PostMessage stays on REST : it's the write path, idempotent, with clean
// HTTP error codes. The resulting events come back through Socket.IO.
func (s *ChatScreen) PostMessage(content string) tea.Cmd {
	c := s.client
	appID := s.appID
	sid := s.sessionID
	mode := s.activeModeID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.PostMessage(ctx, appID, sid, content, mode); err != nil {
			return messagePostedMsg{err: err}
		}
		return messagePostedMsg{}
	}
}

// sendOrQueue posts content now when the session is idle, or holds it in a local
// FIFO while a turn is in flight. Queuing client-side keeps each message its own
// turn : firing a rapid burst at the daemon would let its single-flight coalesce
// them into one reply. pendingTurn is set optimistically so a fast follow-up
// queues instead of racing the turn_started event.
func (s *ChatScreen) sendOrQueue(content string) tea.Cmd {
	s.lastSent = content // remembered so Ctrl+R can retry a failed turn
	if s.pendingTurn {
		s.sendQueue = append(s.sendQueue, content)
		// No toast : the queue depth shows persistently in the composer footer
		// ("⋯ N queued") while it drains, which is the real state to surface.
		return s.ensureTick()
	}
	s.pendingTurn = true
	return s.PostMessage(content)
}

// retryLast re-sends the last user message — the keyboard path to recover a
// turn that failed (LLM/gateway error) without retyping. No-op while a turn is
// in flight or when nothing has been sent yet.
func (s *ChatScreen) retryLast() tea.Cmd {
	if s.pendingTurn || strings.TrimSpace(s.lastSent) == "" {
		return nil
	}
	s.addToast(toastInfo, "retrying…")
	return tea.Batch(s.sendOrQueue(s.lastSent), s.ensureTick())
}

// ---- transcript search (Ctrl+F) -----------------------------------

func (s *ChatScreen) enterSearch() {
	s.searching = true
	s.searchQuery = ""
	s.searchMatches = nil
	s.searchPos = 0
	s.input.Blur() // the composer is inert while searching ; keystrokes build the query
}

func (s *ChatScreen) exitSearch() {
	s.searching = false
	s.searchQuery = ""
	s.searchMatches = nil
	s.messages.ClearSelection()
	s.input.Focus()
}

// runSearch recomputes the matching message indices for the current query and
// jumps to the first match. Case-insensitive ; searches content + tool arg/output.
func (s *ChatScreen) runSearch() {
	s.searchMatches = nil
	q := strings.ToLower(strings.TrimSpace(s.searchQuery))
	if q == "" {
		s.messages.ClearSelection()
		return
	}
	for i, m := range s.messagesSnapshot() {
		if strings.Contains(strings.ToLower(m.Content+"\n"+m.ToolArg+"\n"+m.ToolOutput), q) {
			s.searchMatches = append(s.searchMatches, i)
		}
	}
	if len(s.searchMatches) == 0 {
		s.messages.ClearSelection()
		return
	}
	s.searchPos = 0
	s.messages.SelectIndex(s.searchMatches[0])
}

// searchStep moves to the next (+1) / previous (-1) match, wrapping around.
func (s *ChatScreen) searchStep(d int) {
	n := len(s.searchMatches)
	if n == 0 {
		return
	}
	s.searchPos = ((s.searchPos+d)%n + n) % n
	s.messages.SelectIndex(s.searchMatches[s.searchPos])
}

// welcomeCard is the centred panel shown in a fresh session (no messages yet) :
// the app name, model + active mode, and the key shortcuts to get going.
func (s *ChatScreen) welcomeCard(width int) string {
	th := s.theme
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(th.TextMuted))
	title := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Primary)).Bold(true).Render(s.appName)

	meta := []string{}
	if s.model != "" {
		meta = append(meta, "model "+s.model)
	}
	if i := s.modeIdx; i >= 0 && i < len(s.modes) {
		meta = append(meta, "mode "+s.modes[i].Label)
	}
	lines := []string{title}
	if len(meta) > 0 {
		lines = append(lines, muted.Render(strings.Join(meta, "  ·  ")))
	}
	lines = append(lines, "")

	key := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Accent)).Bold(true)
	tip := func(k, d string) string { return key.Render(k) + muted.Render("  "+d) }
	lines = append(lines, muted.Italic(true).Render("type a message to start"))
	lines = append(lines, "")
	lines = append(lines, tip("/help", "all commands"))
	if len(s.modes) > 1 {
		lines = append(lines, tip("⇧⇥   ", "switch mode"))
	}
	lines = append(lines, tip("^f   ", "search transcript"))

	body := lipgloss.JoinVertical(lipgloss.Center, lines...)
	bw := width - 8
	if bw > 56 {
		bw = 56
	}
	if bw < 24 {
		bw = 24
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(th.BorderSubtle)).
		Padding(1, 3).
		Width(bw).
		Align(lipgloss.Center).
		Render(body)
}

// overlayBottomRight paints pill onto the bottom-right of a rendered area
// (areaW wide), preserving the content underneath to its left. ANSI-aware so
// styled transcript lines aren't corrupted.
func overlayBottomRight(area string, areaW int, pill string) string {
	lines := strings.Split(area, "\n")
	if len(lines) == 0 {
		return area
	}
	x := areaW - lipgloss.Width(pill) - 1
	if x < 0 {
		x = 0
	}
	li := len(lines) - 1
	left := ansi.Truncate(lines[li], x, "")
	if w := lipgloss.Width(left); w < x {
		left += strings.Repeat(" ", x-w)
	}
	lines[li] = left + pill
	return strings.Join(lines, "\n")
}

// searchBar renders the one-line search prompt shown above the (blurred) input
// while search mode is active : "search <query>   X/Y   ↑↓ nav · ↵ next · esc".
// No background fill (concatenated styled segments would break a wrapping bg) —
// just coloured text on the terminal background.
func (s *ChatScreen) searchBar(width int) string {
	th := s.theme
	label := lipgloss.NewStyle().Foreground(lipgloss.Color(th.Primary)).Bold(true).Render("🔍 ")
	var query string
	if s.searchQuery == "" {
		query = lipgloss.NewStyle().Foreground(lipgloss.Color(th.TextMuted)).Italic(true).Render("type to search…")
	} else {
		query = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Text)).Render(s.searchQuery)
	}
	info := ""
	if s.searchQuery != "" {
		if len(s.searchMatches) == 0 {
			info = lipgloss.NewStyle().Foreground(lipgloss.Color(th.Warning)).Render("no matches")
		} else {
			info = lipgloss.NewStyle().Foreground(lipgloss.Color(th.TextMuted)).
				Render(fmt.Sprintf("%d/%d", s.searchPos+1, len(s.searchMatches)))
		}
	}
	hint := lipgloss.NewStyle().Foreground(lipgloss.Color(th.TextMuted)).Faint(true).
		Render("↑↓ nav · ↵ next · esc close")
	left := label + query
	right := info
	if right != "" {
		right += "  "
	}
	right += hint
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// handleSearchKey routes a keystroke while search mode is active. Returns true
// when it consumed the key (always, except it lets nothing fall through).
func (s *ChatScreen) handleSearchKey(key string) {
	switch key {
	case "esc":
		s.exitSearch()
	case "enter", "down", "ctrl+n":
		s.searchStep(+1)
	case "up", "ctrl+p":
		s.searchStep(-1)
	case "backspace":
		if r := []rune(s.searchQuery); len(r) > 0 {
			s.searchQuery = string(r[:len(r)-1])
			s.runSearch()
		}
	case "ctrl+u":
		s.searchQuery = ""
		s.runSearch()
	default:
		if r := []rune(key); len(r) == 1 && unicode.IsPrint(r[0]) {
			s.searchQuery += key
			s.runSearch()
		}
	}
}

// dequeueSend posts the next queued message once the session is idle again. It
// runs after every envelope, so a turn that just ended (pendingTurn cleared by
// turn_ended) immediately kicks the next queued message. No-op when busy or empty.
func (s *ChatScreen) dequeueSend() tea.Cmd {
	if s.pendingTurn || len(s.sendQueue) == 0 {
		return nil
	}
	next := s.sendQueue[0]
	s.sendQueue = s.sendQueue[1:]
	s.pendingTurn = true
	return s.PostMessage(next)
}

// waitForEnvelope blocks on the env channel and returns the next envelope
// as a tea.Msg. The Update loop re-schedules it after each envelopeMsg to
// keep listening. This is the canonical Bubble Tea pattern for bridging a
// channel into the program's message loop.
func waitForEnvelope(ch <-chan client.Envelope) tea.Cmd {
	return func() tea.Msg {
		env, ok := <-ch
		if !ok {
			return nil
		}
		if env.Type == "__conn__" {
			st := client.ConnStateConnecting
			if v, ok := env.Payload["state"].(int); ok {
				st = client.ConnState(v)
			}
			var err error
			if v, ok := env.Payload["err"].(string); ok && v != "" {
				err = fmt.Errorf("%s", v)
			}
			return connStateMsg{state: st, err: err}
		}
		return envelopeMsg{env: env}
	}
}

// ---- layout --------------------------------------------------------

// sidebarFraction is the proportion of the chat-screen width allocated
// to the right rail. Recomputed on every SetSize so the sidebar scales
// with terminal width — wider terminals get more sidebar (more room
// for the timeline, future session list, etc.).
const sidebarFraction = 0.20

// sidebarMin is the floor below which the sidebar would feel cramped
// (counter + key hints stop fitting). On narrow terminals we keep this
// minimum and shrink the chat body instead.
const sidebarMin = 24

// bodySidebarGap is the empty column count between the chat body's
// right edge and the sidebar's thick left border. Visually separates
// the textarea's rounded border from the sidebar's thick one.
const bodySidebarGap = 1

func computeSidebarWidth(totalWidth int) int {
	w := int(float64(totalWidth) * sidebarFraction)
	if w < sidebarMin {
		w = sidebarMin
	}
	return w
}

func (s *ChatScreen) SetSize(w, h int) {
	s.width = w
	s.height = h

	sidebarW := computeSidebarWidth(w)
	bodyW := w - sidebarW - bodySidebarGap
	if bodyW < 30 {
		bodyW = 30
	}
	s.input.SetWidth(bodyW)
	inputH := s.input.Height()
	bodyH := h - inputH
	if bodyH < 3 {
		bodyH = 3
	}
	s.messages.SetSize(bodyW, bodyH)
	s.sidebar.SetSize(sidebarW, h)
}

func (s *ChatScreen) View() string {
	if s.width == 0 || s.height == 0 {
		return ""
	}
	// Force the messages block to fill all the vertical space above the
	// input. Padding the messages (not the whole body) guarantees the
	// input sits at the BOTTOM of the chat area instead of floating up
	// against the few messages. As a side benefit the body now reaches
	// exactly s.height rows so the sidebar's thick left border runs
	// from top to statusbar with no visual short-circuit.
	bodyW := s.width - computeSidebarWidth(s.width) - bodySidebarGap
	if bodyW < 30 {
		bodyW = 30
	}
	// Bottom block : the composer input. While a tool call awaits
	// approval, the approval card is stacked JUST ABOVE the input
	// (input stays put at the very bottom). The transcript stays
	// visible above the card, so the user keeps full context.
	s.input.SetQueued(len(s.sendQueue)) // footer shows "⋯ N queued" while the queue drains
	bottomBlock := s.input.View()
	if s.searching {
		bottomBlock = lipgloss.JoinVertical(lipgloss.Left, s.searchBar(bodyW), s.input.View())
	} else if s.askForm != nil {
		bottomBlock = lipgloss.JoinVertical(lipgloss.Left, s.askForm.Card(bodyW), s.input.View())
	} else if s.approval != nil {
		bottomBlock = lipgloss.JoinVertical(lipgloss.Left, s.approval.Card(bodyW), s.input.View())
	}
	msgsH := s.height - lipgloss.Height(bottomBlock)
	if msgsH < 1 {
		msgsH = 1
	}
	// The messages viewport is sized for the input-height layout
	// (SetSize). When the bottom block is taller than the input — the
	// approval card — its View() would overflow msgsH and push the card
	// off the bottom edge. Clamp to the last msgsH lines (most recent
	// transcript) so the card always sits fully above the status bar.
	var messagesFilled string
	if s.loadingSession {
		// Opening a session : show a spinner while its backlog replays in, then
		// reveal the whole transcript at once (replay_done clears the flag) — the
		// user shouldn't watch messages flicker in one event at a time.
		glyph := shimmerGlyphs[(s.shimmerFrame/shimmerSlow)%len(shimmerGlyphs)]
		spinner := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Primary)).Render(glyph) +
			lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted)).Render("  loading session…")
		messagesFilled = lipgloss.Place(bodyW, msgsH, lipgloss.Center, lipgloss.Center,
			spinner, lipgloss.WithWhitespaceChars(" "))
	} else if s.messages.Empty() && s.streamBuf == "" {
		// Fresh session : a centred welcome card instead of a blank viewport.
		messagesFilled = lipgloss.Place(bodyW, msgsH, lipgloss.Center, lipgloss.Center,
			s.welcomeCard(bodyW), lipgloss.WithWhitespaceChars(" "))
	} else {
		rendered := s.messages.View()
		if lines := strings.Split(rendered, "\n"); len(lines) > msgsH {
			rendered = strings.Join(lines[len(lines)-msgsH:], "\n")
		}
		messagesFilled = lipgloss.NewStyle().
			Width(bodyW).
			MaxWidth(bodyW).
			Height(msgsH).
			Render(rendered)
		// Scrolled up : a "↓ N new" pill on the last row invites a jump to the
		// latest (End when the composer is empty).
		if !s.messages.AtBottom() {
			label := "↓ latest"
			if n := s.messages.BelowCount(); n > 0 {
				label = fmt.Sprintf("↓ %d new", n)
			}
			pill := lipgloss.NewStyle().
				Foreground(lipgloss.Color(s.theme.Background)).
				Background(lipgloss.Color(s.theme.Primary)).
				Bold(true).
				Render(" " + label + " ")
			messagesFilled = overlayBottomRight(messagesFilled, bodyW, pill)
		}
	}
	// Slash autocomplete overlay : render JUST ABOVE the input, replacing
	// the lower portion of the messages area. Suppressed during approval
	// (the input is inert then). Position by trimming N rows from the
	// messages and stacking the popup, then input.
	if s.askForm == nil && s.approval == nil && s.complActive && len(s.complFiltered) > 0 {
		popup := s.renderCompletionPopup(bodyW)
		popupH := lipgloss.Height(popup)
		// Trim the bottom of the messages block to make room.
		msgsLines := strings.Split(messagesFilled, "\n")
		keep := len(msgsLines) - popupH
		if keep < 1 {
			keep = 1
		}
		messagesFilled = strings.Join(msgsLines[:keep], "\n")
		bottomBlock = lipgloss.JoinVertical(lipgloss.Left, popup, s.input.View())
	} else if s.askForm == nil && s.approval == nil && s.shimmering() {
		// Waiting indicator : a shimmer line just above the input while a
		// turn is in flight and no assistant tokens have landed yet. Trim
		// one message row to keep the overall height stable.
		shimmer := lipgloss.NewStyle().Width(bodyW).Render(s.renderShimmer())
		// A blank line above the shimmer so the working indicator isn't glued to
		// the message that ends just above it. Trim two message rows (the blank +
		// the shimmer) to keep the overall height stable.
		msgsLines := strings.Split(messagesFilled, "\n")
		if len(msgsLines) > 2 {
			messagesFilled = strings.Join(msgsLines[:len(msgsLines)-2], "\n")
		}
		bottomBlock = lipgloss.JoinVertical(lipgloss.Left, "", shimmer, s.input.View())
	}
	body := lipgloss.JoinVertical(lipgloss.Left, messagesFilled, bottomBlock)
	// Pin the body to EXACTLY bodyW : Width pads short lines, MaxWidth
	// clips any that overrun (a wide tool-output line, an unwrapped
	// token). Without the clip the body's right edge is ragged, so the
	// sidebar — joined to that edge — drifts left into the chat on the
	// short rows. A fixed edge keeps the sidebar (and its rules) put.
	body = lipgloss.NewStyle().Width(bodyW).MaxWidth(bodyW).Render(body)
	// 1 col gap between the body's right edge and the sidebar's thick
	// left border, else they read as a double border `│┃`.
	body = lipgloss.NewStyle().MarginRight(bodySidebarGap).Render(body)

	stats := SidebarStats{
		AppName:     s.appName,
		Workdir:     s.workdir,
		Phase:       s.currentPhase,
		PendingTurn: s.pendingTurn,
		Todos:       s.todos,
		SubAgents:   s.subAgentViews(),
		Approval:    s.pendingApproval,
		SpinFrame:   s.messages.SpinFrame(),
		Mode:        s.activeModeID(),
		Version:     s.appVersion,
	}
	base := lipgloss.JoinHorizontal(lipgloss.Top, body, s.sidebar.View(stats))

	// Picker overlay : if active, render on top of the chat. The picker
	// is fullscreen-ish (rounded card centered in the chat area).
	if s.help {
		return renderHelp(s.theme, s.width, s.height)
	}
	if s.picker != nil {
		s.picker.SetSize(s.width, s.height)
		if s.picker.Loading() {
			s.picker.SetSpin(s.shimmerFrame)
		}
		return s.picker.View()
	}
	return s.overlayToasts(base)
}

// ---- update --------------------------------------------------------

func (s *ChatScreen) Update(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case sessionCreatedMsg:
		if m.err != nil {
			client.Debugf("chat: sessionCreated err=%v", m.err)
			s.fail("session", m.err)
			return nil
		}
		client.Debugf("chat: sessionCreated id=%s", m.sessionID)
		s.sessionID = m.sessionID
		if m.workdir != "" {
			s.workdir = m.workdir
		} else if m.workspace != "" {
			s.workdir = m.workspace
		}
		// No REST warm-start : the replay since=0 emitted by JoinSession
		// streams the full history through Socket.IO. Removing the warm-start
		// also removes a nasty race where a stale snapshot could clobber
		// realtime-appended messages.
		cmds := []tea.Cmd{
			s.connectRealtime(),
			waitForEnvelope(s.envCh),
			// Now that the session id is known, refetch modes so the switcher
			// restores this session's sticky active mode (FetchModes reads it).
			s.FetchModes(),
		}
		if m.resume {
			// A resumed session has a backlog : spinner until it finishes replaying.
			s.loadingSession = true
			cmds = append(cmds, s.scheduleSessionLoadTimeout(), s.ensureTick())
		}
		return tea.Batch(cmds...)

	case realtimeReadyMsg:
		if m.err != nil {
			// The initial connect failed. socket.io's auto-reconnect only kicks in
			// AFTER a first successful connect, so nothing recovers this on its own
			// — we retry ourselves (daemon not up yet, restarting, or a transient
			// handshake miss). Show "reconnecting", warn once, and schedule a retry.
			s.connState = client.ConnStateReconnecting
			s.lastErr = m.err
			s.loadingSession = false // don't spin on the transcript while the link is down
			s.connRetrying = true // status-bar conn dot shows "◌ reconnecting…"
			return tea.Batch(s.scheduleConnectRetry(), s.ensureTick())
		}
		// Connected (Connect + JoinSession both succeeded). Set the state directly
		// here rather than waiting for the transient __conn__ event, which can be
		// delayed or dropped under load — that gap is what showed "not connected"
		// on a live link. Subsequent drops/recoveries still flow via __conn__.
		s.connRetrying = false // recovered ; the conn dot flips back to ● connected
		if s.connStop != nil {
			close(s.connStop)
		}
		if s.rt != nil {
			_ = s.rt.Close() // supersede any prior connection (e.g. a retry that raced a switch)
		}
		s.rt = m.rt
		s.connStop = m.stop
		s.connState = client.ConnStateConnected
		// No waitForEnvelope here : the one issued alongside connectRealtime is
		// still blocked on envCh (it never returned during the outage) and is
		// re-armed by each connStateMsg/envelopeMsg — adding another would create
		// a duplicate listener that splits events.
		return nil

	case connectRetryMsg:
		// Give up retrying once we've recovered or there's no session to join yet.
		if s.connState == client.ConnStateConnected || s.sessionID == "" {
			return nil
		}
		return s.connectRealtime()

	case sessionLoadTimeoutMsg:
		// Stale timer (we've since switched away) or already revealed : ignore.
		if m.sessionID == s.sessionID && s.loadingSession {
			s.loadingSession = false
			s.messages.GotoBottom()
		}
		return nil

	case connStateMsg:
		s.connState = m.state
		if m.err != nil {
			s.lastErr = m.err
		}
		// No toast : the connection state lives in the status bar's conn dot
		// (●/◌/✗ + label). The stream resumes automatically — the realtime layer
		// re-joins the session and replays since lastSeq on every reconnect.
		return tea.Batch(waitForEnvelope(s.envCh), s.ensureTick())

	case envelopeMsg:
		s.handleEnvelope(m.env)
		// A turn that just ended (pendingTurn cleared by turn_ended) releases
		// the next locally-queued message, one per turn.
		return tea.Batch(waitForEnvelope(s.envCh), s.dequeueSend(), s.ensureTick())

	case animTickMsg:
		s.pruneToasts()
		s.advanceActivity()
		s.shimmerFrame++
		// Re-render the scrollback so a running tool/agent chip's live duration
		// ticks up. The per-message cache makes this cheap : only the running
		// chip(s) actually re-render, every committed line is reused.
		s.messages.RefreshRunning()
		// Animate the live tool overlay's spinner while tools are in flight (it is
		// only repainted on set/clear otherwise, so its glyph would sit frozen).
		if len(s.streamingToolIDs) > 0 {
			s.refreshStreamingTools()
		}
		if s.animating() {
			return s.tickCmd()
		}
		s.ticking = false
		return nil

	case messagePostedMsg:
		// A failed POST means the turn never even started — surface it, clear
		// the optimistic pendingTurn so the spinner/queue don't wedge, and let
		// the next queued message (if any) take its turn.
		if m.err != nil {
			s.fail("send failed", m.err)
			s.pendingTurn = false
			return s.dequeueSend()
		}
		// On success pendingTurn stays set (optimistically on send, confirmed by
		// turn_started) and clears on turn_ended.
		return nil

	case modelLoadedMsg:
		s.model = m.model
		return nil

	case appInfoMsg:
		if m.name != "" {
			s.appName = m.name
		}
		s.appVersion = m.version
		return nil

	case modesLoadedMsg:
		s.applyModes(m.modes, m.active)
		return nil

	case abortResultMsg:
		if m.err != nil {
			s.addToast(toastError, "interrupt failed: "+m.err.Error())
			return s.ensureTick()
		}
		return nil

	case approvalResolvedMsg:
		// The daemon's approval_granted/denied envelope clears the modal
		// and writes the timeline entry. On a REST failure, re-enable the
		// modal so the user can retry instead of being stuck.
		if m.err != nil && s.approval != nil {
			s.lastErr = m.err
			s.approval.submitting = false
			s.approval.decision = ""
			s.appendInlineSystem("approval failed: " + m.err.Error())
		}
		if m.err != nil && s.askForm != nil {
			s.lastErr = m.err
			s.askForm.submitting = false
			s.appendInlineSystem("answer failed: " + m.err.Error())
		}
		return nil

	case tea.KeyMsg:
		// Help overlay eats the next key (any key dismisses it).
		if s.help {
			s.help = false
			return nil
		}

		// ask_user form eats all keys while a question awaits an answer.
		// Esc cancels (denies, empty answer) ; submit sends the assembled
		// reply back as the approval reason.
		if s.askForm != nil {
			if s.askForm.submitting {
				return nil
			}
			action := s.askForm.Update(msg)
			switch {
			case action.Cancelled:
				s.askForm.submitting = true
				return s.resolveAsk("denied", "")
			case action.Submit:
				s.askForm.submitting = true
				return s.resolveAsk("approved", action.Answer)
			}
			return nil
		}

		// Approval modal eats all keys while a tool call awaits a
		// decision. Deny is the conservative default for esc.
		if s.approval != nil {
			if s.approval.submitting {
				return nil
			}
			switch m.String() {
			case "y", "a":
				s.approval.submitting = true
				s.approval.decision = "approved"
				return s.resolveApproval("approved")
			case "n", "d", "esc":
				s.approval.submitting = true
				s.approval.decision = "denied"
				return s.resolveApproval("denied")
			}
			return nil
		}

		// Picker overlay eats all keys while active.
		if s.picker != nil {
			action := s.picker.Update(msg)
			if action.Cancelled {
				s.picker = nil
				s.pickerKind = ""
				return nil
			}
			if action.Selected != "" {
				kind := s.pickerKind
				selected := action.Selected
				s.picker = nil
				s.pickerKind = ""
				return s.handlePickerSelection(kind, selected)
			}
			if action.Deleted != "" {
				// Keep the picker open ; the list refreshes once the delete
				// completes (sessionDeletedMsg).
				return s.deleteSession(action.Deleted)
			}
			return nil
		}

		// Transcript search owns the keyboard while active : typed runes build the
		// query, ↑/↓ (or ^p/^n) walk matches, enter = next, esc closes.
		if s.searching {
			s.handleSearchKey(m.String())
			return nil
		}
		// Ctrl+F opens transcript search ; Ctrl+R retries the last sent message
		// (recover a failed turn without retyping).
		if m.String() == "ctrl+f" {
			s.enterSearch()
			return nil
		}
		if m.String() == "ctrl+r" {
			return s.retryLast()
		}
		// End (when the composer is empty) jumps the transcript to the latest —
		// the keyboard counterpart to the "↓ N new" pill. Non-empty composer keeps
		// End as move-cursor-to-line-end.
		if m.String() == "end" && strings.TrimSpace(s.input.Value()) == "" {
			s.messages.GotoBottom()
			return nil
		}

		// Message-selection navigation : move a highlight cursor through
		// the transcript without leaving the composer. Intercepted before
		// the textarea so they don't insert / move the text cursor.
		if !s.complActive {
			switch m.String() {
			case "ctrl+p":
				s.messages.SelectPrev()
				return nil
			case "ctrl+n":
				s.messages.SelectNext()
				return nil
			}
		}
		// Enter on a selected sub-agent chip drills into its child session
		// (its full isolated transcript) ; esc returns to the parent.
		if m.String() == "enter" && s.messages.HasSelection() {
			if sel, ok := s.messages.SelectedMessage(); ok {
				if child := s.agentChild[sel.CallID]; child != "" {
					return s.drillIntoChild(child)
				}
			}
		}
		// Esc : leave a drilled-in sub-agent, else clear a selection, else
		// interrupt an in-flight turn.
		if m.String() == "esc" && !s.complActive {
			if s.drillParent != "" {
				return s.popDrill()
			}
			if s.messages.HasSelection() {
				s.messages.ClearSelection()
				return nil
			}
			if s.pendingTurn {
				return s.abortTurn()
			}
		}
		// Ctrl+Y yanks the selected message — or the latest reply when none
		// is selected — to the system clipboard (OSC52, works over SSH).
		if m.String() == "ctrl+y" {
			return s.copyMessage()
		}
		// Ctrl+O expands / collapses all tool results — the keyboard path
		// to output details (no mouse needed, e.g. over SSH).
		if m.String() == "ctrl+o" {
			s.messages.ToggleAllTools()
			return nil
		}
		// Shift+Tab cycles the composer mode (runtime.modes), Claude-Code style.
		// The active mode is sent with each message and shown in the input footer.
		if m.String() == "shift+tab" && !s.complActive && len(s.modes) > 1 {
			s.cycleMode()
			return nil
		}

		// Slash-command autocomplete is active : intercept navigation
		// + Enter + Esc BEFORE the input sees them, so they don't end
		// up as text in the buffer.
		if s.complActive {
			switch m.String() {
			case "esc":
				s.complActive = false
				s.complCursor = 0
				return nil
			case "up", "ctrl+p":
				if s.complCursor > 0 {
					s.complCursor--
				}
				return nil
			case "down", "ctrl+n":
				if s.complCursor < len(s.complFiltered)-1 {
					s.complCursor++
				}
				return nil
			case "tab":
				// Tab : autocomplete the typed prefix to the selected command.
				if len(s.complFiltered) > 0 {
					name := s.complFiltered[s.complCursor].Name
					s.replaceInputWith("/" + name + " ")
					s.refreshCompletion()
				}
				return nil
			case "enter":
				// Enter : execute the selected command immediately.
				if len(s.complFiltered) > 0 {
					name := s.complFiltered[s.complCursor].Name
					s.input.ta.Reset()
					s.complActive = false
					s.complCursor = 0
					return s.dispatchCommand("/" + name)
				}
				return nil
			}
		}

		// Reaching the composer means the user is typing again : drop any
		// message selection so the highlight doesn't linger.
		s.messages.ClearSelection()
		inputCmd := s.input.Update(msg)
		// After the input has consumed the key, sync the completion
		// state with what's now in the buffer.
		s.refreshCompletion()
		if content, ok := s.input.Submit(); ok {
			s.complActive = false
			s.complCursor = 0
			s.input.Remember(content)
			// Slash command typed without going through the autocomplete
			// path (e.g. pasted) : intercept BEFORE sending to the daemon.
			if strings.HasPrefix(content, "/") {
				return tea.Batch(inputCmd, s.dispatchCommand(content))
			}
			return tea.Batch(inputCmd, s.sendOrQueue(content))
		}
		// Do NOT forward the keystroke to the messages viewport : its default
		// keymap scrolls on plain letters (j/k/u/d/f/b/space/g/G) and arrows,
		// so typing those into a message would scroll the chat under you. The
		// transcript scrolls via the mouse wheel (tea.MouseMsg, handled
		// separately) and PageUp/PageDown only — neither conflicts with typing.
		var vpCmd tea.Cmd
		if k := m.String(); k == "pgup" || k == "pgdown" {
			vpCmd = s.messages.Update(msg)
		}
		return tea.Batch(inputCmd, vpCmd)

	case pickerItemsMsg:
		if m.err != nil {
			s.picker = nil // drop the spinner — the load failed
			s.fail("load", m.err)
			return nil
		}
		// Fill the loading picker in place when its rows arrive ; otherwise
		// (callers that don't pre-open a spinner, e.g. /theme) build a fresh one.
		if s.picker != nil && s.picker.Loading() {
			s.picker.title = m.title
			s.picker.SetItems(m.items, m.deletable)
		} else {
			s.picker = NewPicker(s.theme, m.title, m.items)
			s.picker.deletable = m.deletable
		}
		s.pickerKind = m.kind
		return nil

	case sessionDeletedMsg:
		if m.err != nil {
			s.fail("delete session", m.err)
			return s.ensureTick()
		}
		s.addToast(toastSuccess, "session deleted")
		if m.wasCurrent {
			// Deleted the session we were in : drop the picker and open a
			// fresh one so the chat isn't pointing at a dead session.
			s.picker = nil
			s.pickerKind = ""
			return tea.Batch(s.newSession(), s.ensureTick())
		}
		// Refresh the list in place so the deleted row disappears.
		return tea.Batch(s.loadSessionsPicker(), s.ensureTick())

	case switchSessionMsg:
		if m.err != nil {
			s.fail("switch", m.err)
			return nil
		}
		s.workdir = m.workdir
		return s.applySessionSwitch(m.sessionID)

	case quitMsg:
		return tea.Quit

	case tea.PasteMsg:
		// Bracketed paste : drop the content into the composer. Ignored
		// while a modal owns the keyboard (approval / picker). Refresh
		// the slash-completion in case the paste starts with "/".
		if s.approval != nil || s.picker != nil {
			return nil
		}
		s.input.Insert(m.Content)
		s.refreshCompletion()
		return nil

	case tea.MouseClickMsg:
		// Left-click toggles a collapsed tool result. Only inside the
		// messages area, and not while a modal/popup is up (those shift the
		// row↔content mapping). Anything else falls through to the viewport
		// (wheel scroll etc.).
		if m.Button == tea.MouseLeft && s.approval == nil && !s.complActive &&
			m.X < s.messages.Width() && m.Y < s.messages.Height() {
			if s.messages.ToggleAt(m.Y) {
				return nil
			}
		}
		return s.messages.Update(msg)

	case tea.MouseMsg:
		return s.messages.Update(msg)
	}
	return nil
}

// handleEnvelope is the heart of the realtime path. Routes the daemon's
// session events onto the chat state — messages, turn lifecycle, tool calls.
// Anything not understood is shown in the timeline as a generic entry.
func (s *ChatScreen) handleEnvelope(env client.Envelope) {
	client.Debugf("chat: handleEnvelope type=%s seq=%d session=%s active=%s", env.Type, env.Seq, env.SessionID, s.sessionID)
	s.ensureAgentMaps()

	// Dedup : on reconnect the live room (join_session) and replay-since overlap,
	// so the same (session, seq) can arrive twice. Skip anything we've already
	// processed. Keyed per session id because root and each sub-session have
	// independent seq counters. seq==0 events (transient, e.g. streaming deltas)
	// are never persisted/replayed, so they bypass dedup.
	if env.Seq > 0 {
		if last, ok := s.seenSeq[env.SessionID]; ok && env.Seq <= last {
			client.Debugf("chat: dropping duplicate envelope session=%s seq=%d", env.SessionID, env.Seq)
			return
		}
		s.seenSeq[env.SessionID] = env.Seq
	}

	// A sub-agent's OWN turn activity, fanned out by the daemon bridge from its
	// isolated sub-session to this (root) session's room. It carries the
	// sub-session id in SessionID — which would trip the cross-session guard
	// below — so handle it HERE, first, and route it to the attributed
	// sub-agent trace. It must NEVER flow into the main timeline / streaming
	// bubble (that would splice the sub-agent's tokens into the coordinator's
	// message). Only accept it for the active root session.
	if env.AgentRunID != "" && env.RootSessionID != "" && env.RootSessionID == s.sessionID {
		s.handleSubAgentActivity(env)
		return
	}

	// Ignore late envelopes from a session we just switched away from.
	// The realtime stream may have buffered events emitted before we
	// rejoined ; routing them onto the new session's view would mix
	// histories.
	if env.SessionID != "" && s.sessionID != "" && env.SessionID != s.sessionID {
		client.Debugf("chat: dropping cross-session envelope")
		return
	}
	// End of the session backlog : the full history has streamed in, so reveal
	// the transcript (drop the loading spinner) and pin it to the latest.
	if env.Type == "replay_done" {
		if s.loadingSession {
			s.loadingSession = false
			s.messages.GotoBottom()
		}
		return
	}

	switch env.Type {
	case "user_message", "assistant_message", "system_message":
		s.appendMessageFromEnvelope(env)
		// The final assistant message supersedes the streamed preview :
		// clear the transient bubble so the markdown-rendered message
		// shows in its place (no duplicate).
		if env.Type == "assistant_message" {
			s.streamBuf = ""
			s.messages.SetStreaming("")
			// The round's stream is over : clear the live thinking trace (the
			// consolidated reasoning rides this message, rendered as a 💭 block).
			s.thinkBuf = ""
			s.messages.SetThinking("")
			// Do NOT clear the live tool overlay here. The round's TEXT stream is
			// over, but its tools are about to EXECUTE — they must stay in the live
			// overlay (a stable area above the input) so the user keeps seeing what
			// is running until each tool's result lands (cleared per call_id then).
			// A fast tool's pending→result window is too short to perceive as an
			// inline chip alone, which is why running tools looked like they were
			// skipped straight to the result. Turn start/end still clear the overlay
			// wholesale, so a stray streamed fragment that never became a real tool
			// can't leak past the turn.
			// A round's message just committed : bank its output tokens so the next
			// round's live count adds on top instead of restarting from zero.
			s.turnRounds++
			s.turnTokensBase = s.turnTokens
		}

	case "assistant_delta":
		// R-4 token streaming : show the in-progress text live as RAW markdown
		// source (renderStreaming wraps it without glamour, so no garbled partial
		// render). This keeps the agent's intermediate narration visible while it
		// works ; every message still renders to full glamour the instant it
		// commits (renderAssistant).
		delta := payloadPartsText(env.Payload)
		s.streamBuf += delta
		s.turnChars += len([]rune(delta)) // local fallback if the daemon sends no count
		if env.LiveOutputTokens > 0 {
			// LiveOutputTokens is per-message ; add it to the rounds already banked
			// this turn so the counter never drops back when a new round starts.
			s.turnTokens = s.turnTokensBase + env.LiveOutputTokens
		}
		s.messages.SetStreaming(s.streamBuf)

	case "assistant_reasoning_delta":
		// The agent's thinking streams live : accumulate + render it dimmed
		// above the answer. Transient ; the consolidated reasoning lands on the
		// final assistant_message (rendered there as a collapsed 💭 block).
		s.thinkBuf += payloadStr(env.Payload, "reasoning")
		s.messages.SetThinking(s.thinkBuf)

	case "turn_started":
		s.pendingTurn = true
		s.currentPhase = "running"
		s.turnChars = 0           // fresh token counter for the new turn
		s.turnTokens = 0          // fresh live token count for the new turn
		s.turnTokensBase = 0      // no rounds banked yet
		s.turnRounds = 0
		s.turnTokensExact = false // estimate until the provider usage lands
		s.subActivity = nil       // reset any sub-agent activity groups
		s.compacting = false // fresh compaction state for the new turn
		s.thinkBuf = ""      // fresh thinking trace for the new turn
		s.messages.SetThinking("")

	case "turn_phase_changed":
		phase := ""
		if v, ok := env.Payload["phase"].(string); ok {
			phase = v
		}
		s.currentPhase = phase

	case "token_usage", "cost_update":
		// The provider's EXACT usage landed (CTX-7.1 anchor). tokens_out carries
		// only the LAST round's completion, not the turn total — so snap to it
		// (dropping the ~ marker) only for a single-round turn. For a multi-round
		// turn the cumulative live estimate is the better whole-turn figure, so we
		// keep it (still ~). Either way never let it drop below what we've shown.
		if v, ok := env.Payload["tokens_out"].(float64); ok && v > 0 {
			if s.turnRounds <= 1 {
				s.turnTokens = int(v)
				s.turnTokensExact = true
			} else if int(v) > s.turnTokens {
				s.turnTokens = int(v)
			}
		}

	case "context_tokens":
		// The background Context Service recounted the EXACT occupancy (CTX-7).
		// total = tokens in the window now ; window = the model's context window
		// (configured max_tokens, else model default). Feed the composer's
		// occupancy gauge — it climbs as we work and drops on compaction.
		if v, ok := env.Payload["total"].(float64); ok {
			s.ctxUsed = int(v)
		}
		if v, ok := env.Payload["window"].(float64); ok && v > 0 {
			s.ctxWindow = int(v)
		}
		s.input.SetContext(s.ctxUsed, s.ctxWindow)

	case "context_compacting":
		// Compaction started : nothing in the chat thread — the live "in progress"
		// indicator rides next to the spinner above the input (renderShimmer).
		s.compacting = true

	case "context_compacted":
		// Compaction finished : inject a subtle one-line summary into the chat
		// thread at THIS event's seq, so it sits where the compaction actually
		// happened (between the tool calls and the reply) and scrolls up with the
		// conversation instead of being pinned to the bottom. The live "in
		// progress" indicator above the input clears.
		s.compacting = false
		// A compaction that applied nothing (dropped == 0) — aborted by the user or
		// a no-op — only clears the "compacting…" indicator : no chat note, no gauge
		// change (the context is exactly as it was).
		if payloadInt(env.Payload, "messages_dropped") <= 0 {
			return
		}
		freed := payloadInt(env.Payload, "tokens_freed")
		summary := "compaction complete"
		if freed > 0 {
			summary = fmt.Sprintf("compaction complete · %s tokens freed", humanizeTokens(int(freed)))
		}
		// Post-compaction occupancy for the note's "ctx used/window" : the daemon's
		// FULL occupancy (system prompt + tool schemas + kept messages incl. surviving
		// tool calls/results), on the SAME scale as the window and the live footer —
		// not a misleadingly tiny messages-only figure. Race-free (read from THIS
		// event). Falls back to the message-only estimate for an older daemon.
		used := payloadInt(env.Payload, "new_context_tokens")
		if used <= 0 {
			used = payloadInt(env.Payload, "tokens_before") - freed
		}
		gauge := ""
		if used > 0 && s.ctxWindow > 0 {
			gauge = fmt.Sprintf("ctx %s/%s", humanizeTokens(int(used)), humanizeTokens(s.ctxWindow))
		}
		// Drop the LIVE footer gauge to the post-compaction occupancy NOW. The
		// daemon's authoritative new_context_tokens is in THIS event, so the gauge
		// reflects the freed context immediately instead of staying stale (often
		// >100%, e.g. "16.9k/16k") until the next context_tokens event lands after
		// turn_ended.
		if used > 0 {
			s.ctxUsed = int(used)
			s.input.SetContext(s.ctxUsed, s.ctxWindow)
		}
		s.injectCompaction(env, summary, gauge)

	case "turn_ended":
		s.pendingTurn = false
		s.currentPhase = ""
		// Drop any leftover streaming preview (e.g. a turn that errored
		// mid-stream and never emitted a final assistant_message).
		s.streamBuf = ""
		s.messages.SetStreaming("")
		s.thinkBuf = ""
		s.messages.SetThinking("")
		s.clearAllStreamingTools()
		// The turn is over : nothing more will resolve a chip, so any chip
		// still on "running" is stale — mark it ended (no perpetual spinner).
		s.messages.FinalizeRunning()
		// Same for the activity rail : freeze the main + sub-agent groups so
		// an interrupted turn doesn't keep spinning their tools indefinitely.
		s.finalizeActivity()
		s.compacting = false
		// turn_ended carries status ∈ {done, errored, interrupted} +
		// optional reason. On non-done, we surface the reason as a
		// visible error message so the user actually knows the turn
		// failed (vs silently waiting for a reply that never comes).
		status := payloadStr(env.Payload, "status")
		reason := payloadStr(env.Payload, "reason")
		switch status {
		case "errored":
			s.injectMessage("error", friendlyError(reason))
		case "interrupted":
			s.injectMessage("error", friendlyInterrupt(reason))
		}

	case "session_interrupted":
		// The user aborted. This is the reliable abort signal — it always
		// follows a /abort even if the turn's own turn_ended is delayed or
		// (in a wedged turn) never arrives. Tear the live UI down here so the
		// flow visibly stops : drop the pending turn, kill the stream preview,
		// and freeze every running chip + activity row. Harmless if turn_ended
		// already did it (all the operations are idempotent).
		s.pendingTurn = false
		s.currentPhase = ""
		s.streamBuf = ""
		s.messages.SetStreaming("")
		s.messages.FinalizeRunning()
		s.finalizeActivity()

	case "message_started", "message_done":
		// A fresh assistant message round begins : reset the streaming
		// buffer so a new round's tokens don't append to the previous
		// one (matters for multi-round turns with tool calls between).
		if env.Type == "message_started" {
			s.streamBuf = ""
			s.messages.SetStreaming("")
		}

	case "tool_call":
		// The `agent` delegation tool is the canonical sub-agent chip
		// (opencode-style) : "agent <kind> <task>". The redundant
		// agent_spawn/agent_result event chips are suppressed below.
		if kind, task, ok := agentSpawnArgs(env.Payload); ok {
			callID := payloadStr(env.Payload, "call_id")
			s.ensureAgentMaps()
			s.agentCalls[callID] = true // recognise the result by call_id
			// Remember this chip so the upcoming agent_spawn (which carries the
			// run_id but not the call_id) can bind its run_id to it, and the live
			// sub-agent tool count lands on the right chip.
			s.lastAgentCallByKind[kind] = callID
			// The streaming overlay showed "Agent … · N tok" while the model typed
			// this delegation call ; the real agent chip now takes over, so drop the
			// overlay line — otherwise the two duplicate each other in the chat.
			s.clearStreamingTool(callID)
			// No timeline marker here : the agent_spawn handler pushes the single
			// "◇ <kind>" marker. Pushing it from BOTH duplicated it in the rail.
			s.upsertChip(env, "agent", callID, kind, "running", 0, oneLine(task, 64), "", "")
			break
		}
		name := toolDisplayName(env.Payload)
		if isHiddenTool(stripModulePrefix(name)) {
			break // memory bookkeeping (todos/goal/remember) : not a chip
		}
		// Streaming phase : the model is still emitting this call. Show the tool
		// name + a growing token counter in the LIVE bottom area (below the
		// streaming text), NOT as a timeline chip — a streaming event's seq is
		// lower than the assistant_message that announces it, so a timeline chip
		// would sort ahead of the intent. The ordered chip is created later from
		// the durable pending event (correct seq).
		if payloadStr(env.Payload, "status") == "streaming" {
			// Emission phase, BEFORE the real call : show the chip-styled line
			// (spinner + tool name). The live token count is NOT shown per-line —
			// it feeds the single central working indicator above the input.
			s.setStreamingTool(payloadStr(env.Payload, "call_id"), name)
			if env.LiveOutputTokens > 0 {
				s.turnTokens = s.turnTokensBase + env.LiveOutputTokens
			}
			break
		}
		callID := payloadStr(env.Payload, "call_id")
		if stripModulePrefix(name) == "run_parallel" {
			// run_parallel is a long-running meta-tool whose inline chip shows live
			// per-child progress (tool_progress) — that chip IS the running
			// indicator, so drop the overlay for it.
			s.clearStreamingTool(callID)
			s.ensureAgentMaps()
			if callID != "" {
				s.parallelArgs[callID] = parallelTaskArgs(effectiveToolArgs(env.Payload))
			}
		} else {
			// HAND OFF : the durable pending call now creates the real running chip
			// (same chip style, same position the streaming line occupied), so drop
			// the streaming overlay here — keeping it would duplicate the tool (chip
			// in the timeline AND the line below it) and make the transition jump.
			s.clearStreamingTool(callID)
		}
		// The ordered inline chip is created running at the pending seq (so it sorts
		// after the intent message and carries the call's arg hint) — it takes over
		// from the streaming line IN PLACE and settles when the result lands.
		s.upsertToolChip(env, callID, name, "running", 0, toolArgHint(env.Payload), "", "")

	case "tool_result":
		// Sub-agent delegation result : the payload is the agent snapshot ;
		// parse it for the REAL sub-agent status + telemetry + summary/error
		// (the tool call itself succeeds even when the sub-agent errors).
		if callID := payloadStr(env.Payload, "call_id"); s.agentCalls[callID] {
			// Matched by call_id (robust to the execute_tool wrapper, whose
			// result payload has no arguments to unwrap). The payload is the
			// finished sub-agent snapshot : show its REAL status + telemetry.
			snap := parseAgentSnapshot(env.Payload)
			kind := snapStr(snap, "agent_id")
			status := snapStr(snap, "status")
			if status == "" {
				status = payloadStr(env.Payload, "status")
			}
			s.removeSubActivity(snapStr(snap, "run_id")) // sub-agent done : drop its group
			s.upsertChip(env, "agent", callID, kind,
				status, snapInt(snap, "duration_ms"), "", agentSnapBody(snap), "")
			// Correlate this call to its run_id (for a later async
			// agent_result) and its child session (captured on spawn, for the
			// drill-in).
			if rid := snapStr(snap, "run_id"); rid != "" {
				s.agentRunCall[rid] = callID
				if child := s.agentRunChild[rid]; child != "" {
					s.agentChild[callID] = child
				}
			}
			break
		}
		name := toolDisplayName(env.Payload)
		if isHiddenTool(stripModulePrefix(name)) {
			break // memory bookkeeping (todos/goal/remember) : not a chip
		}
		// The result landed : drop the tool's live overlay line (kept there through
		// emission AND execution). The settled, ordered chip is upserted below — it
		// is CREATED here as completed for a normal tool (no inline chip existed
		// while it ran), or UPDATED for run_parallel (which had a running chip).
		s.clearStreamingTool(payloadStr(env.Payload, "call_id"))
		// run_parallel : header = "N actions · names" derived from the result ;
		// expanded body = each sub-tool with its captured arg (file/command),
		// status and any error — so opening the group shows the details of each
		// parallel call, like a simple call. Other tools keep the generic path.
		resultArg, output := "", formatToolResult(env.Payload)
		if stripModulePrefix(name) == "run_parallel" {
			resultArg = parallelResultLabel(env.Payload)
			output = s.parallelGroupBody(env.Payload, payloadStr(env.Payload, "call_id"))
		}
		s.upsertToolChip(env, payloadStr(env.Payload, "call_id"), name,
			payloadStr(env.Payload, "status"), payloadInt(env.Payload, "duration_ms"),
			resultArg, output, toolDiffText(env.Payload))

	case "tool_progress":
		// A run_parallel child finished : advance the parent chip's live
		// "N/total done" so the batch visibly progresses BEFORE the combined
		// result lands. The event carries the parent's call_id in correlation_id.
		// Cosmetic only — this event is not in the agent's history, so its
		// barrier result is unchanged. Generic : works for every child tool.
		if parent := env.CorrelationID; parent != "" {
			if done, total := parallelProgressCounts(env.Payload); total > 0 {
				s.setParallelProgress(parent, fmt.Sprintf("%d/%d done", done, total))
			}
		}

	case "agent_spawn":
		// Suppressed as a chip (the `agent` tool-call chip is canonical) ;
		// we only record the child session id for the drill-in, and a
		// timeline marker.
		if rid := payloadStr(env.Payload, "run_id"); rid != "" {
			s.ensureAgentMaps()
			k := payloadStr(env.Payload, "kind")
			if k != "" {
				s.agentRunKind[rid] = k
			}
			// Bind run_id → the EXACT delegation chip. The daemon now carries the
			// originating call_id (parent_call_id), so this is deterministic even
			// for parallel / same-kind delegations — where the old "last call of
			// this kind" guess mislinked, so a sub-agent finishing updated the
			// wrong chip (or none until the final tool_result). Fallback kept for
			// an older daemon that doesn't send parent_call_id.
			if pc := payloadStr(env.Payload, "parent_call_id"); pc != "" {
				s.agentRunCall[rid] = pc
			} else if cid := s.lastAgentCallByKind[k]; cid != "" {
				s.agentRunCall[rid] = cid
			}
			if child := payloadStr(env.Payload, "child_session_id"); child != "" {
				s.agentRunChild[rid] = child
			}
			// Open the sub-agent's own activity group (pinned header + bounded
			// tools) — NOT a flat timeline marker that the tool flood would evict.
			s.addSubActivity(rid, k)
		}

	case "agent_result":
		// Finalise the chip ONLY when it maps to a known agent tool-call
		// (async delegation : wait=false / background_run, whose completion
		// arrives here by run_id rather than as a tool_result). No mapping =
		// the call is represented by another chip ; don't create a duplicate.
		rid := payloadStr(env.Payload, "run_id")
		s.removeSubActivity(rid) // the sub-agent finished : drop its group
		if cid := s.agentRunCall[rid]; cid != "" {
			detail := payloadStr(env.Payload, "result_summary")
			if detail == "" {
				detail = payloadStr(env.Payload, "error")
			}
			s.upsertChip(env, "agent", cid, payloadStr(env.Payload, "kind"),
				payloadStr(env.Payload, "status"), payloadInt(env.Payload, "duration_ms"), "", detail, "")
		}

	case "todo_added":
		s.applyTodoAdded(env)
	case "todo_updated":
		s.applyTodoUpdated(env)

	case "approval_request":
		// kind=="question" is an ask_user prompt (free text / choices / form) ;
		// everything else is an SG-5 tool-call gate (y/a/n). They render as two
		// different controls and answer through two different paths.
		if payloadStr(env.Payload, "kind") == "question" {
			if q := NewAskForm(s.theme, env.Payload); q != nil {
				s.askForm = q
				s.input.Blur()
				s.pendingApproval = "question: " + orDash(payloadStr(env.Payload, "reason"))
			}
		} else if p := NewApprovalPrompt(s.theme, env.Payload); p != nil {
			s.approval = p
			s.pendingApproval = "approval: " + orDash(payloadStr(env.Payload, "tool_name"))
			// Flip the gated tool's chip to a distinct "awaiting approval" state
			// (not a running spinner — it isn't running, it's waiting on the user).
			s.approvalCallID = payloadStr(env.Payload, "call_id")
			s.setChipStatus(s.approvalCallID, "awaiting")
		}

	case "approval_granted", "approval_denied":
		id := payloadStr(env.Payload, "id")
		if s.approval != nil && (id == "" || id == s.approval.ID()) {
			s.approval = nil
		}
		if s.askForm != nil && (id == "" || id == s.askForm.ID()) {
			s.askForm = nil
			s.input.Focus()
		}
		// Restore the gated chip : granted → it actually runs now ("running") so
		// the user SEES it execute after approving ; denied → it never ran.
		if s.approvalCallID != "" {
			if env.Type == "approval_granted" {
				s.setChipStatus(s.approvalCallID, "running")
			} else {
				s.setChipStatus(s.approvalCallID, "ended")
			}
			s.approvalCallID = ""
		}
		// The pending approval is resolved : clear it so the activity panel shows
		// only the CURRENT one (no growing list of "✓ approved" lines).
		s.pendingApproval = ""

	case "error":
		msg := payloadStr(env.Payload, "error")
		if msg == "" {
			msg = payloadStr(env.Payload, "message")
		}
		if msg == "" {
			msg = payloadStr(env.Payload, "detail")
		}
		if msg == "" {
			msg = "error"
		}
		s.injectMessage("error", msg)
	}
}

// fail records an error AND surfaces it in the transcript, so a failure
// is never swallowed into s.lastErr where the user can't see it. context
// is a short prefix ("send failed", "session"). friendlyError maps the
// common gateway/timeout/connection cases to actionable wording.
func (s *ChatScreen) fail(context string, err error) {
	if err == nil {
		return
	}
	s.lastErr = err
	s.injectMessage("error", context+": "+friendlyError(err.Error()))
}

// injectMessage is the SINGLE entry point for client-only content that
// has no daemon sequence — system notes, errors, and (later) context
// dumps, summaries, anything we surface in the transcript ourselves.
//
// It anchors the message at the current bottom (the highest seq present)
// so a stable sort keeps it just below the last message AT INJECTION TIME,
// while every later real message — which always carries a strictly higher
// daemon seq — sorts BELOW it. That's the whole reason injected content
// scrolls up with the conversation instead of getting pinned to the
// bottom. Any new "inject X into the chat" feature must go through here,
// never invent its own seq.
func (s *ChatScreen) injectMessage(role, content string) {
	if content == "" {
		return
	}
	existing := s.messagesSnapshot()
	existing = append(existing, client.Message{
		Role:       role,
		Content:    content,
		Seq:        maxMsgSeq(existing),
		TsUnixNano: time.Now().UnixNano(),
	})
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].Seq < existing[j].Seq })
	s.messages.SetMessages(existing)
}

// injectCompaction drops the end-of-compaction summary into the thread as a
// subtle one-line note (NOT a tool chip), anchored at the compaction event's
// own daemon seq. That seq sits between the tool calls that filled the window
// and the assistant reply that follows, so the note lands where the compaction
// actually happened and scrolls up with the conversation — never pinned to the
// bottom. summary is the left text ("N tokens libérés") ; gauge the right ctx
// readout ("ctx used/window", "" to omit). Falls back to the bottom anchor if
// the event somehow carries no seq.
func (s *ChatScreen) injectCompaction(env client.Envelope, summary, gauge string) {
	existing := s.messagesSnapshot()
	seq := env.Seq
	if seq == 0 {
		seq = maxMsgSeq(existing)
	}
	existing = append(existing, client.Message{
		Role:       "compaction",
		Content:    summary,
		ToolArg:    gauge,
		Seq:        seq,
		TsUnixNano: parseEnvelopeTs(env.Ts).UnixNano(),
	})
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].Seq < existing[j].Seq })
	s.messages.SetMessages(existing)
}

// maxMsgSeq returns the highest Seq in the list (0 when empty). The anchor
// for injectMessage.
func maxMsgSeq(msgs []client.Message) uint64 {
	var mx uint64
	for i := range msgs {
		if msgs[i].Seq > mx {
			mx = msgs[i].Seq
		}
	}
	return mx
}

// friendlyError turns raw runtime error strings into something a human can
// act on. Mainly catches the LLM gateway 401 path and prompts to re-login.
func friendlyError(reason string) string {
	if reason == "" {
		return "the assistant turn failed"
	}
	low := strings.ToLower(reason)
	switch {
	case strings.Contains(low, "401") && (strings.Contains(low, "provider") || strings.Contains(low, "gateway") || strings.Contains(low, "bifrost")):
		return "gateway rejected your token (401) — run `digitorn login` to renew"
	case strings.Contains(low, "context deadline exceeded"), strings.Contains(low, "timeout"):
		return "LLM call timed out — try again, or check the gateway"
	case strings.Contains(low, "connection refused"), strings.Contains(low, "no such host"):
		return "couldn't reach the LLM gateway — is it running ?"
	}
	return reason
}

func friendlyInterrupt(reason string) string {
	if reason == "" {
		return "turn interrupted"
	}
	return "interrupted : " + reason
}

// appendMessageFromEnvelope materializes a client.Message from the envelope
// payload and either updates an existing message (same seq) or appends a new
// one. SetMessages re-sorts internally so we can hand a partial list.
func (s *ChatScreen) appendMessageFromEnvelope(env client.Envelope) {
	role := ""
	switch env.Type {
	case "user_message":
		role = "user"
	case "assistant_message":
		role = "assistant"
	case "system_message":
		role = "system"
		// Behavior directives ([BEHAVIOR REMINDER/WARNING/BLOCKED], the
		// classifier) are injected to steer the model, not for the user to
		// read. They still ride in the LLM context daemon-side ; here we just
		// don't paint them in the chat. Keyed on extra.source so it survives
		// any wording change to the directive text itself.
		if isHiddenDirective(env.Payload) {
			return
		}
	}
	content := payloadStr(env.Payload, "content")
	if content == "" {
		return
	}
	msg := client.Message{
		Role:       role,
		Content:    content,
		Reasoning:  payloadStr(env.Payload, "reasoning"),
		Seq:        env.Seq,
		TsUnixNano: parseEnvelopeTs(env.Ts).UnixNano(),
	}
	// Merge with existing list by seq, preserving order.
	existing := s.messagesSnapshot()
	replaced := false
	for i := range existing {
		if existing[i].Seq == msg.Seq && msg.Seq > 0 {
			existing[i] = msg
			replaced = true
			break
		}
	}
	if !replaced {
		existing = append(existing, msg)
	}
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].Seq < existing[j].Seq })
	s.messages.SetMessages(existing)
}

// setChipStatus flips an existing tool/agent chip (by call_id) to a new status
// in place — used to mark a chip "awaiting" while its approval modal is up and
// "running" once granted. No-op if the chip isn't present yet (graceful on a
// realtime race) or the status is unchanged.
func (s *ChatScreen) setChipStatus(callID, status string) {
	if callID == "" {
		return
	}
	msgs := s.messagesSnapshot()
	for i := range msgs {
		if (msgs[i].Role == "tool" || msgs[i].Role == "agent") && msgs[i].CallID == callID {
			if msgs[i].Status == status {
				return
			}
			msgs[i].Status = status
			s.messages.SetMessages(msgs)
			return
		}
	}
}

// upsertToolChip is the tool-role shorthand for upsertChip.
func (s *ChatScreen) upsertToolChip(env client.Envelope, callID, name, status string, durationMs int64, arg, output, diff string) {
	s.upsertChip(env, "tool", callID, name, status, durationMs, arg, output, diff)
}

// isTransientEnvelope reports whether an event is a high-frequency, droppable
// UI hint (vs a durable event carrying real state). Used by the realtime
// overflow valve : under a flood, only these may be dropped — a message, a
// tool result or a final tool_call must always get through.
func isTransientEnvelope(env client.Envelope) bool {
	switch env.Type {
	case "assistant_delta", "assistant_reasoning_delta", "context_tokens":
		return true
	case "tool_call":
		return payloadStr(env.Payload, "status") == "streaming"
	}
	return false
}

// streamingTool is one tool call still being emitted by the model, shown live
// as a chip-styled line UNTIL its durable pending event hands off to the ordered
// timeline chip.
type streamingTool struct {
	name string
}

// setStreamingTool upserts a live streaming tool (by call_id) and repaints.
// Position is insertion order ; updates are in place.
func (s *ChatScreen) setStreamingTool(callID, name string) {
	if callID == "" {
		return
	}
	if s.streamingTools == nil {
		s.streamingTools = map[string]*streamingTool{}
	}
	st := s.streamingTools[callID]
	if st == nil {
		st = &streamingTool{}
		s.streamingTools[callID] = st
		s.streamingToolIDs = append(s.streamingToolIDs, callID)
	}
	st.name = name
	s.refreshStreamingTools()
}

// clearStreamingTool drops one live streaming line (its real chip is taking
// over). No-op if absent.
func (s *ChatScreen) clearStreamingTool(callID string) {
	if callID == "" || s.streamingTools[callID] == nil {
		return
	}
	delete(s.streamingTools, callID)
	for i, id := range s.streamingToolIDs {
		if id == callID {
			s.streamingToolIDs = append(s.streamingToolIDs[:i], s.streamingToolIDs[i+1:]...)
			break
		}
	}
	s.refreshStreamingTools()
}

// clearAllStreamingTools drops every live streaming line (turn ended / reset).
func (s *ChatScreen) clearAllStreamingTools() {
	if len(s.streamingToolIDs) == 0 {
		return
	}
	s.streamingTools = map[string]*streamingTool{}
	s.streamingToolIDs = nil
	s.refreshStreamingTools()
}

// refreshStreamingTools renders the live streaming-tool lines into the bottom
// area, below the streaming text — never in the seq'd timeline.
func (s *ChatScreen) refreshStreamingTools() {
	if len(s.streamingToolIDs) == 0 {
		s.messages.SetStreamingTools("")
		return
	}
	// Render each in-emission tool as a chip-identical line (same spinner, verb,
	// left bar, panel bg) so it morphs seamlessly into the real running chip when
	// the call lands — no muted-overlay-then-chip jump. The live token count
	// stays centralised on the working indicator above the input (renderShimmer).
	var lines []string
	for _, id := range s.streamingToolIDs {
		st := s.streamingTools[id]
		if st == nil {
			continue
		}
		lines = append(lines, s.messages.StreamingToolLine(st.name, s.shimmerFrame))
	}
	if len(lines) == 0 {
		s.messages.SetStreamingTools("")
		return
	}
	s.messages.SetStreamingTools("\n" + strings.Join(lines, "\n"))
}

// upsertChip inserts or updates an inline chip (a tool call or a sub-agent
// run) in the message stream. The first event creates it ("running") at
// its own seq, so it sits between the user message and the reply ; the
// matching terminal event updates the SAME chip in place (by call/run id)
// with status + duration + output. An unknown id appends a fresh chip so
// nothing is silently dropped.
func (s *ChatScreen) ensureAgentMaps() {
	if s.seenSeq == nil {
		s.seenSeq = map[string]uint64{}
	}
	if s.agentCalls == nil {
		s.agentCalls = map[string]bool{}
	}
	if s.agentRunCall == nil {
		s.agentRunCall = map[string]string{}
	}
	if s.agentChild == nil {
		s.agentChild = map[string]string{}
	}
	if s.agentRunChild == nil {
		s.agentRunChild = map[string]string{}
	}
	if s.agentRunKind == nil {
		s.agentRunKind = map[string]string{}
	}
	if s.parallelArgs == nil {
		s.parallelArgs = map[string][]string{}
	}
	if s.agentToolCount == nil {
		s.agentToolCount = map[string]int{}
	}
	if s.lastAgentCallByKind == nil {
		s.lastAgentCallByKind = map[string]string{}
	}
}

// handleSubAgentActivity renders the live activity of a delegated sub-agent,
// fanned out by the daemon from its isolated sub-session to this root session.
// It is attributed to the sub-agent's logical id (kind, learned at agent_spawn)
// and appended to the timeline as an indented trace — deliberately NOT into the
// main message stream (that would splice the sub-agent's tokens into the
// coordinator's reply). High-volume events (token deltas, turn lifecycle,
// message_started/done) are consumed silently so the trace stays readable : the
// point is to show WHAT the sub-agent is doing, live, without flooding.
func (s *ChatScreen) handleSubAgentActivity(env client.Envelope) {
	kind := s.agentRunKind[env.AgentRunID]
	if kind == "" {
		kind = "agent"
	}
	switch env.Type {
	case "tool_call":
		label := stripModulePrefix(toolDisplayName(env.Payload))
		if isHiddenTool(label) {
			return // memory bookkeeping : not shown in the activity panel
		}
		// Streaming frames repeat the same call_id : skip them here so they never
		// inflate the sub-agent's tool count (it's tallied once, on the completed
		// call below). The live streaming counter is rendered on the main chip.
		if payloadStr(env.Payload, "status") == "streaming" {
			return
		}
		if arg := toolArgHint(env.Payload); arg != "" {
			label += " " + arg
		}
		s.subActivityTool(env.AgentRunID, kind, payloadStr(env.Payload, "call_id"), label)
		// Live tool count for this sub-agent : tally each fanned tool_call and
		// surface the running total on its delegation chip.
		s.ensureAgentMaps()
		s.agentToolCount[env.AgentRunID]++
		if cid := s.agentRunCall[env.AgentRunID]; cid != "" {
			s.setAgentToolCount(cid, s.agentToolCount[env.AgentRunID])
		}
	case "tool_result":
		if isHiddenTool(stripModulePrefix(toolDisplayName(env.Payload))) {
			return
		}
		s.subActivityComplete(env.AgentRunID, payloadStr(env.Payload, "call_id"),
			stripModulePrefix(toolDisplayName(env.Payload)), timelineToolOK(env.Payload))
	}
}

func (s *ChatScreen) upsertChip(env client.Envelope, role, callID, name, status string, durationMs int64, arg, output, diff string) {
	if name == "" {
		name = callID
	}
	existing := s.messagesSnapshot()
	for i := range existing {
		if existing[i].Role == role && callID != "" && existing[i].CallID == callID {
			existing[i].Status = status
			existing[i].DurationMs = durationMs
			// keep whichever field the current event carries, never blank
			// the other (spawn carries arg, result carries output/diff).
			if arg != "" {
				existing[i].ToolArg = arg
			}
			if output != "" {
				existing[i].ToolOutput = output
			}
			if diff != "" {
				existing[i].ToolDiff = diff
			}
			s.messages.SetMessages(existing)
			return
		}
	}
	existing = append(existing, client.Message{
		Role:       role,
		Content:    name,
		CallID:     callID,
		Status:     status,
		Seq:        env.Seq,
		DurationMs: durationMs,
		ToolArg:    arg,
		ToolOutput: output,
		ToolDiff:   diff,
		TsUnixNano: parseEnvelopeTs(env.Ts).UnixNano(),
	})
	sort.SliceStable(existing, func(i, j int) bool { return existing[i].Seq < existing[j].Seq })
	s.messages.SetMessages(existing)
}

// setParallelProgress updates the live "N/total done" hint on a run_parallel
// chip (matched by call_id) as each child finishes. No-op if the chip isn't
// present (e.g. a stray progress event), so it never creates a phantom chip.
func (s *ChatScreen) setParallelProgress(callID, hint string) {
	if callID == "" {
		return
	}
	existing := s.messagesSnapshot()
	for i := range existing {
		if existing[i].Role == "tool" && existing[i].CallID == callID {
			existing[i].ToolArg = hint
			s.messages.SetMessages(existing)
			return
		}
	}
}

// parallelProgressCounts reads {completed,total} from a tool_progress event's
// nested metadata. Returns (0,0) when absent.
func parallelProgressCounts(p map[string]any) (done, total int) {
	m, _ := p["metadata"].(map[string]any)
	if m == nil {
		return 0, 0
	}
	toInt := func(v any) int {
		switch n := v.(type) {
		case float64:
			return int(n)
		case int:
			return n
		case int64:
			return int(n)
		}
		return 0
	}
	return toInt(m["completed"]), toInt(m["total"])
}

// setAgentToolCount updates the live tool count on an agent chip (by call_id)
// in place, so the running "· N tools" reflects the sub-agent's progress. A
// no-op if the chip isn't present yet.
func (s *ChatScreen) setAgentToolCount(callID string, n int) {
	if callID == "" {
		return
	}
	existing := s.messagesSnapshot()
	for i := range existing {
		if existing[i].Role == "agent" && existing[i].CallID == callID {
			if existing[i].ToolCount == n {
				return
			}
			existing[i].ToolCount = n
			s.messages.SetMessages(existing)
			return
		}
	}
}

// applyTodoAdded records a task created by the agent (memory.task_create) and
// refreshes the inline todo block. Updates in place if the id is already known.
func (s *ChatScreen) applyTodoAdded(env client.Envelope) {
	id := payloadStr(env.Payload, "id")
	if id == "" {
		return
	}
	text := payloadStr(env.Payload, "text")
	status := payloadStr(env.Payload, "status")
	if status == "" {
		status = "pending"
	}
	for i := range s.todos {
		if s.todos[i].ID == id {
			if text != "" {
				s.todos[i].Text = text
			}
			s.todos[i].Status = status
			s.syncTodoBlock(env)
			return
		}
	}
	s.todos = append(s.todos, todoItem{ID: id, Text: text, Status: status})
	s.syncTodoBlock(env)
}

// applyTodoUpdated flips a task's status (memory.task_update). The update event
// carries no text, so the title captured at creation is preserved. Tolerates an
// update that arrives before its create (creates the task then).
func (s *ChatScreen) applyTodoUpdated(env client.Envelope) {
	id := payloadStr(env.Payload, "id")
	if id == "" {
		return
	}
	status := payloadStr(env.Payload, "status")
	text := payloadStr(env.Payload, "text")
	for i := range s.todos {
		if s.todos[i].ID == id {
			if status != "" {
				s.todos[i].Status = status
			}
			if text != "" {
				s.todos[i].Text = text
			}
			s.syncTodoBlock(env)
			return
		}
	}
	if status == "" {
		status = "pending"
	}
	s.todos = append(s.todos, todoItem{ID: id, Text: text, Status: status})
	s.syncTodoBlock(env)
}

// syncTodoBlock upserts the single inline "todo" block (a checklist that updates
// in place at its original position) from the current task list.
func (s *ChatScreen) syncTodoBlock(env client.Envelope) {
	if len(s.todos) == 0 {
		return
	}
	s.upsertChip(env, "todo", "__todos__", "Tasks", "", 0, "", renderTodoLines(s.todos, s.theme, s.messages.Width()-3), "")
}

// isAgentTool reports whether a tool event is the `agent` delegation tool.
func isAgentTool(p map[string]any) bool {
	return stripModulePrefix(toolDisplayName(p)) == "agent"
}

// agentSpawnArgs extracts (kind, task) from an `agent` SPAWN call's args.
// ok is false for non-agent tools or non-spawn agent calls (list/status/
// cancel carry no delegation target).
func agentSpawnArgs(p map[string]any) (kind, task string, ok bool) {
	if !isAgentTool(p) {
		return "", "", false
	}
	args := effectiveToolArgs(p)
	if args == nil {
		return "", "", false
	}
	if kind, _ = args["agent"].(string); kind == "" {
		kind, _ = args["specialist"].(string)
	}
	if kind == "" {
		return "", "", false
	}
	if task, _ = args["task"].(string); task == "" {
		task, _ = args["prompt"].(string)
	}
	return kind, task, true
}

// parseAgentSnapshot decodes the agent tool's result (a JSON snapshot of the
// finished sub-agent) into a map. nil when the payload isn't JSON.
func parseAgentSnapshot(p map[string]any) map[string]any {
	raw := strings.TrimSpace(payloadPartsText(p))
	if raw == "" {
		raw = strings.TrimSpace(payloadStr(p, "output"))
	}
	if raw == "" {
		return nil
	}
	var m map[string]any
	if json.Unmarshal([]byte(raw), &m) != nil {
		return nil
	}
	return m
}

func snapStr(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

func snapInt(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	}
	return 0
}

// agentSnapBody builds a sub-agent chip's expandable body : a telemetry line
// then the result content (or the error, on failure).
func agentSnapBody(m map[string]any) string {
	if m == nil {
		return ""
	}
	var meta []string
	if n := snapInt(m, "tool_calls"); n > 0 {
		meta = append(meta, fmt.Sprintf("%d tools", n))
	}
	if in, out := snapInt(m, "tokens_in"), snapInt(m, "tokens_out"); in > 0 || out > 0 {
		meta = append(meta, fmt.Sprintf("%s↑ %s↓ tok", humanizeTokens(int(in)), humanizeTokens(int(out))))
	}
	var b strings.Builder
	if len(meta) > 0 {
		b.WriteString(strings.Join(meta, " · ") + "\n\n")
	}
	if e := snapStr(m, "error"); e != "" {
		b.WriteString(e)
	} else if c := snapStr(m, "content"); c != "" {
		b.WriteString(c)
	}
	return strings.TrimSpace(b.String())
}

// effectiveToolArgs returns the real argument map of a tool call,
// unwrapping the execute_tool indirection (the target's args live in
// arguments.params).
func effectiveToolArgs(p map[string]any) map[string]any {
	args, _ := p["arguments"].(map[string]any)
	if args == nil {
		return nil
	}
	if stripModulePrefix(payloadStr(p, "name")) == "execute_tool" {
		if params, ok := args["params"].(map[string]any); ok {
			return params
		}
	}
	return args
}

// toolArgHint picks a one-line hint of the call's key argument, keyed by
// the action : path for read/write/edit/ls/glob, pattern for grep,
// command for bash/exec, else the first string argument.
// baseName returns the last path segment, tolerating both / and \ separators
// (daemon paths are Windows-absolute). Trailing slashes are trimmed first so a
// directory still yields its own name.
func baseName(p string) string {
	p = strings.TrimRight(p, `/\`)
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// cleanToolName turns a sanitised fully-qualified tool name ("bash__run", the
// "__" form the gate layer uses) back into the readable "bash.run".
func cleanToolName(s string) string {
	return strings.ReplaceAll(s, "__", ".")
}

// extractParallelTasks pulls the fan-out list of {tool, args} objects out of a
// run_parallel call's args, tolerating a STRING-encoded JSON array (the model
// often double-encodes : {"tasks":"[{...}]"}) under any of the wrapper keys.
func extractParallelTasks(args map[string]any) []map[string]any {
	asArr := func(v any) []any {
		switch t := v.(type) {
		case []any:
			return t
		case string:
			if s := strings.TrimSpace(t); strings.HasPrefix(s, "[") {
				var a []any
				if json.Unmarshal([]byte(s), &a) == nil {
					return a
				}
			}
		}
		return nil
	}
	var arr []any
	for _, k := range []string{"tasks", "actions", "calls", "tools", "invocations", "steps", "items"} {
		if a := asArr(args[k]); len(a) > 0 {
			arr = a
			break
		}
	}
	if arr == nil {
		for _, v := range args {
			if a := asArr(v); len(a) > 0 {
				arr = a
				break
			}
		}
	}
	var out []map[string]any
	for _, e := range arr {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// parallelTaskName reads a task's target tool name (any accepted key).
func parallelTaskName(task map[string]any) string {
	for _, k := range []string{"tool", "name", "action", "tool_name"} {
		if s, ok := task[k].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// parallelNames joins the sub-tool action names for the chip header.
func parallelNames(args map[string]any) string {
	var names []string
	for _, t := range extractParallelTasks(args) {
		if n := parallelTaskName(t); n != "" {
			names = append(names, stripModulePrefix(cleanToolName(n)))
		}
	}
	return parallelLabel(names)
}

// parallelTaskArgs returns the per-task argument hint (file / command), in input
// order, so the expanded group can show WHAT each parallel sub-tool operated on.
func parallelTaskArgs(args map[string]any) []string {
	tasks := extractParallelTasks(args)
	out := make([]string, len(tasks))
	for i, t := range tasks {
		action := stripModulePrefix(cleanToolName(parallelTaskName(t)))
		var ta map[string]any
		for _, k := range []string{"args", "params", "arguments", "input"} {
			if m, ok := t[k].(map[string]any); ok {
				ta = m
				break
			}
		}
		out[i] = argHintFor(action, ta)
	}
	return out
}

// parallelLabel formats a run_parallel header by aggregating duplicate sub-tool
// names with a "*N" multiplier, first-seen order : [run, glob, glob] →
// "run · glob *2". Distinct names are capped with an ellipsis.
func parallelLabel(names []string) string {
	if len(names) == 0 {
		return ""
	}
	var order []string
	count := map[string]int{}
	for _, n := range names {
		if _, seen := count[n]; !seen {
			order = append(order, n)
		}
		count[n]++
	}
	const max = 4
	overflow := false
	if len(order) > max {
		order = order[:max]
		overflow = true
	}
	parts := make([]string, 0, len(order))
	for _, n := range order {
		if count[n] > 1 {
			parts = append(parts, fmt.Sprintf("%s *%d", n, count[n]))
		} else {
			parts = append(parts, n)
		}
	}
	out := strings.Join(parts, " · ")
	if overflow {
		out += " · …"
	}
	return out
}

// parallelGroupBody renders the expanded run_parallel group : one line per
// sub-tool — its action, the captured arg (which file/command it ran on),
// status glyph and any error — so opening the block shows each parallel call's
// details, like a simple call. Falls back to the plain result when the shape
// isn't recognised.
func (s *ChatScreen) parallelGroupBody(p map[string]any, callID string) string {
	raw := payloadPartsText(p)
	if raw == "" {
		raw = payloadStr(p, "output")
	}
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &m) != nil {
		return formatToolResult(p)
	}
	arr, ok := m["results"].([]any)
	if !ok {
		return formatToolResult(p)
	}
	storedArgs := s.parallelArgs[callID]
	var lines []string
	for i, e := range arr {
		r, _ := e.(map[string]any)
		if r == nil {
			continue
		}
		action := stripModulePrefix(cleanToolName(mapStr(r, "name")))
		glyph := "·"
		switch mapStr(r, "status") {
		case "completed", "done", "ok", "success":
			glyph = "✓"
		case "errored", "error", "failed", "cancelled":
			glyph = "✗"
		}
		line := glyph + " " + action
		if i < len(storedArgs) && storedArgs[i] != "" {
			line += " " + storedArgs[i]
		}
		if errMsg := mapStr(r, "error"); errMsg != "" {
			line += " — " + oneLine(errMsg, 50)
		}
		lines = append(lines, line)
	}
	if len(lines) == 0 {
		return formatToolResult(p)
	}
	return strings.Join(lines, "\n")
}

// parallelResultLabel builds the run_parallel header from its RESULT
// ({results:[{name},…]}) — reliable even when the tool_call args didn't survive
// as a parseable action list.
func parallelResultLabel(p map[string]any) string {
	raw := payloadPartsText(p)
	if raw == "" {
		raw = payloadStr(p, "output")
	}
	var m map[string]any
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &m) != nil {
		return ""
	}
	arr, ok := m["results"].([]any)
	if !ok {
		return ""
	}
	var names []string
	for _, e := range arr {
		if r, ok := e.(map[string]any); ok {
			names = append(names, stripModulePrefix(cleanToolName(mapStr(r, "name"))))
		}
	}
	return parallelLabel(names)
}

func toolArgHint(p map[string]any) string {
	return argHintFor(stripModulePrefix(toolDisplayName(p)), effectiveToolArgs(p))
}

// argHintFor extracts the one-line argument hint for a tool from its action
// name + args. Split out of toolArgHint so run_parallel can reuse it per
// sub-task (each task is a {tool, args} of its own).
func argHintFor(action string, args map[string]any) string {
	if args == nil {
		return ""
	}
	pick := func(keys ...string) string {
		for _, k := range keys {
			if v, ok := args[k].(string); ok && v != "" {
				return v
			}
		}
		return ""
	}
	var h string
	switch action {
	case "read", "write", "edit", "multi_edit":
		// Just the file name : the daemon hands back an absolute workdir path
		// (C:\…\workdirs\app\hash\session\src\App.jsx) which is noise in a chip.
		h = baseName(pick("path", "file_path"))
	case "ls", "glob":
		h = pick("path", "pattern", "file_path")
	case "grep", "search":
		h = pick("pattern", "query")
	case "run", "bash", "exec", "shell":
		h = pick("command", "cmd")
	case "background_run":
		// Launch carries the inner tool/agent (sanitised "bash__run") ; the
		// status/wait/cancel modes carry a task UUID. Show the clean inner name,
		// else a short task id.
		if t := pick("tool", "agent", "kind", "name"); t != "" {
			h = cleanToolName(t)
		} else if id := pick("task_id", "id"); id != "" {
			if i := strings.IndexByte(id, '-'); i > 0 {
				h = id[:i] // first UUID segment, e.g. "b94673e1"
			} else {
				h = shortID(id)
			}
		}
	case "run_parallel":
		// Show the fan-out : the names of the tools launched in parallel.
		h = parallelNames(args)
	default:
		for _, v := range args {
			if sv, ok := v.(string); ok && sv != "" {
				h = sv
				break
			}
		}
	}
	return oneLine(h, 56)
}

// toolDiffText pulls the unified diff a file-mutating tool (edit/write)
// attaches to its result. The daemon carries it client-side on the tool_result
// payload (never in the LLM-visible parts). Empty for non-mutating tools.
func toolDiffText(p map[string]any) string {
	return payloadStr(p, "unified_diff")
}

// oneLine collapses whitespace/newlines to a single line and truncates
// to max runes with an ellipsis.
func oneLine(s string, max int) string {
	s = strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(s, "\n", " "), "\t", " "))
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// toolDisplayName resolves the tool name to show in a chip. When the
// model reaches a tool through the execute_tool meta-indirection, the
// real target lives in arguments.name — show that ("filesystem.write")
// instead of the wrapper ("context_builder.execute_tool"). The tool_call
// event carries arguments ; the matching tool_result updates the chip in
// place by call_id and keeps this resolved name.
func toolDisplayName(p map[string]any) string {
	name := payloadStr(p, "name")
	if stripModulePrefix(name) == "execute_tool" {
		if args, ok := p["arguments"].(map[string]any); ok {
			if target, _ := args["name"].(string); target != "" {
				return target
			}
		}
	}
	return name
}

// payloadInt reads an integer field from an envelope payload, tolerating
// the float64 that JSON numbers decode to.
func payloadInt(p map[string]any, key string) int64 {
	if p == nil {
		return 0
	}
	switch v := p[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// Model exposes the entry agent's configured model for the status bar.
func (s *ChatScreen) Model() string { return s.model }

// messagesSnapshot returns a copy of the current messages list. Used as the
// merge base when integrating a new envelope.
func (s *ChatScreen) messagesSnapshot() []client.Message {
	// Messages widget owns the canonical list ; we re-derive by inspecting
	// its stored slice through SetMessages on the next call. Cheap : the
	// slice is already in memory.
	return append([]client.Message(nil), s.messages.messages...)
}

// isHiddenTool reports whether a tool is memory bookkeeping (todos, goal,
// remember) that should NOT show as a chip in the chat nor as a row in the
// activity panel — the todo list / its own UI represents it instead.
func isHiddenTool(action string) bool {
	switch action {
	case "task_create", "task_update", "set_goal", "remember":
		return true
	}
	return false
}

// timelineToolOK reports whether a tool_result payload represents success, so
// the activity row flips to ✓ rather than ✗.
func timelineToolOK(p map[string]any) bool {
	if payloadStr(p, "error") != "" {
		return false
	}
	switch st := payloadStr(p, "status"); st {
	case "errored", "error", "failed", "cancelled":
		return false
	}
	return true
}

// subAgentViews snapshots the active sub-agent groups for the sidebar.
func (s *ChatScreen) subAgentViews() []SubAgentView {
	if len(s.subActivity) == 0 {
		return nil
	}
	out := make([]SubAgentView, len(s.subActivity))
	for i, sa := range s.subActivity {
		out[i] = SubAgentView{Kind: sa.kind, Finished: sa.finished, Running: sa.running, Settling: sa.settling, Hidden: sa.hidden}
	}
	return out
}

// addSubActivity registers an active sub-agent group (pinned header + tools).
func (s *ChatScreen) addSubActivity(runID, kind string) {
	if runID == "" {
		return
	}
	for _, sa := range s.subActivity {
		if sa.runID == runID {
			if kind != "" {
				sa.kind = kind
			}
			return
		}
	}
	s.subActivity = append(s.subActivity, &subAgentActivity{runID: runID, kind: kind})
}

// removeSubActivity drops a sub-agent's group once it finishes, so the panel
// shows only sub-agents still working.
func (s *ChatScreen) removeSubActivity(runID string) {
	if runID == "" {
		return
	}
	out := s.subActivity[:0]
	for _, sa := range s.subActivity {
		if sa.runID != runID {
			out = append(out, sa)
		}
	}
	s.subActivity = out
}

// addTool records a tool as RUNNING (kept individually with its label so it can
// be shown live). Shared by the main agent and each sub-agent.
func (sa *subAgentActivity) addTool(callID, text string) {
	for i := range sa.running {
		if sa.running[i].CallID == callID && callID != "" {
			sa.running[i].Label = text // re-emitted call : refresh the label
			return
		}
	}
	sa.running = append(sa.running, TimelineEntry{Label: text, CallID: callID})
}

// completeTool moves a tool from running to a finished row (✓/✗), bounded to the
// most recent subActivityFinishedMax (older finished rows scroll into "…").
func (sa *subAgentActivity) completeTool(callID, fallback string, ok bool) {
	label := fallback
	for i := range sa.running {
		if sa.running[i].CallID == callID {
			label = sa.running[i].Label
			sa.running = append(sa.running[:i], sa.running[i+1:]...)
			break
		}
	}
	glyph := "✓"
	if !ok {
		glyph = "✗"
	}
	sa.finished = append(sa.finished, TimelineEntry{Label: glyph + " " + label, CallID: callID, Type: "tool_result"})
	n := len(sa.finished)
	sa.finished = trimFinished(sa.finished)
	sa.hidden += n - len(sa.finished)
	// Leave a fading ghost where the tool was so it eases out instead of
	// vanishing the instant it completes.
	sa.settling = append(sa.settling, TimelineEntry{Label: label, CallID: callID, ok: ok, settle: activitySettleTicks})
}

// finalize converts any still-running tools into "■ interrupted" finished rows so
// a turn that ends — normally or via abort — leaves no perpetual spinner in the
// rail. Idempotent : with nothing running it's a no-op.
func (sa *subAgentActivity) finalize() {
	// Turn's over : drop every live row (spinners) and any fading ghost so
	// nothing keeps animating. The panel resets on the next turn_started.
	sa.running = nil
	sa.settling = nil
}

// advanceActivity ages the rail one animation tick : live rows lose their
// fade-in faintness, and settling ghosts count down and drop at zero. Cheap —
// just slice walks — and only matters while a turn is running (the tick that
// drives it stops when nothing's in flight).
func (s *ChatScreen) advanceActivity() {
	step := func(sa *subAgentActivity) {
		for i := range sa.running {
			sa.running[i].age++
		}
		if len(sa.settling) == 0 {
			return
		}
		kept := sa.settling[:0]
		for _, e := range sa.settling {
			if e.settle--; e.settle > 0 {
				kept = append(kept, e)
			}
		}
		sa.settling = kept
	}
	for _, sa := range s.subActivity {
		step(sa)
	}
}

// finalizeActivity stops every live indicator in the activity rail : each
// sub-agent group's running tools and the pending approval. Called when a turn
// ends so an interrupt (or any turn end that left work mid-flight) doesn't
// leave the sidebar spinning forever.
func (s *ChatScreen) finalizeActivity() {
	for _, sa := range s.subActivity {
		sa.finalize()
	}
	s.pendingApproval = ""
}

// subActivityTool routes a sub-agent tool to its group, creating it if needed.
func (s *ChatScreen) subActivityTool(runID, kind, callID, text string) {
	var sa *subAgentActivity
	for _, g := range s.subActivity {
		if g.runID == runID {
			sa = g
			break
		}
	}
	if sa == nil {
		s.addSubActivity(runID, kind)
		sa = s.subActivity[len(s.subActivity)-1]
	}
	sa.addTool(callID, text)
}

// trimFinished keeps only the most recent subActivityFinishedMax finished rows ;
// the rest are counted into the group's "hidden" total (the "…" hint).
func trimFinished(rows []TimelineEntry) []TimelineEntry {
	if len(rows) <= subActivityFinishedMax {
		return rows
	}
	return rows[len(rows)-subActivityFinishedMax:]
}

// subActivityComplete routes a sub-agent tool result to its group.
func (s *ChatScreen) subActivityComplete(runID, callID, fallback string, ok bool) {
	for _, sa := range s.subActivity {
		if sa.runID == runID {
			sa.completeTool(callID, fallback, ok)
			return
		}
	}
}

// isHiddenDirective reports whether a system_message is an internal directive
// the user shouldn't see — behavior enforcement/classifier notes that exist
// only to steer the model. Keyed on the payload's extra.source so it's immune
// to wording changes in the directive text.
func isHiddenDirective(p map[string]any) bool {
	extra, ok := p["extra"].(map[string]any)
	if !ok {
		return false
	}
	switch extra["source"] {
	case "behavior_enforcement", "behavior_classifier", "mode_switch":
		// behavior notes steer the model ; mode_switch echoes a mode change the
		// footer already shows. None are for the user to read in the transcript.
		return true
	}
	return false
}

func payloadStr(p map[string]any, key string) string {
	if p == nil {
		return ""
	}
	if v, ok := p[key].(string); ok {
		return v
	}
	return ""
}

// payloadPartsText concatenates the text of a message payload's parts
// ({parts:[{type:"text", text:"…"}]}). assistant_delta carries its
// token there rather than in the legacy "content" field.
func payloadPartsText(p map[string]any) string {
	if p == nil {
		return ""
	}
	parts, ok := p["parts"].([]any)
	if !ok {
		return ""
	}
	var b strings.Builder
	for _, it := range parts {
		if m, ok := it.(map[string]any); ok {
			if t, ok := m["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return b.String()
}

func parseEnvelopeTs(s string) time.Time {
	if s == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t
	}
	return time.Now()
}

// relTime renders an ISO-8601 timestamp as a short human age ("just now", "5m
// ago", "2h ago", "3d ago", or a "Jan 2" date past a week). Empty for an empty
// or unparseable timestamp so callers can skip it.
func relTime(iso string) string {
	if iso == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339Nano, iso)
	if err != nil {
		if t, err = time.Parse(time.RFC3339, iso); err != nil {
			return ""
		}
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return "just now"
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

// ---- slash commands -----------------------------------------------

type pickerItemsMsg struct {
	kind      string
	title     string
	items     []PickerItem
	deletable bool
	err       error
}

type sessionDeletedMsg struct {
	sid        string
	wasCurrent bool
	err        error
}

type switchSessionMsg struct {
	sessionID string
	workdir   string
	err       error
}

type quitMsg struct{}

// dispatchCommand parses a slash command and returns the tea.Cmd that
// executes it. Unknown commands surface as an error message in chat.
func (s *ChatScreen) dispatchCommand(line string) tea.Cmd {
	parts := strings.Fields(strings.TrimPrefix(line, "/"))
	if len(parts) == 0 {
		return nil
	}
	cmd := strings.ToLower(parts[0])
	switch cmd {
	case "sessions":
		return s.openSessionsPicker()
	case "apps":
		return s.openAppsPicker()
	case "new":
		return s.newSession()
	case "theme":
		// /theme <name> applies directly ; bare /theme opens the picker.
		if len(parts) > 1 {
			s.applyTheme(parts[1])
			return s.ensureTick()
		}
		return s.loadThemePicker()
	case "help", "?":
		return s.showHelp()
	case "quit", "exit", "q":
		return func() tea.Msg { return quitMsg{} }
	default:
		s.appendInlineSystem(fmt.Sprintf("unknown command: /%s — try /help", cmd))
		return nil
	}
}

func (s *ChatScreen) loadSessionsPicker() tea.Cmd {
	c := s.client
	appID := s.appID
	current := s.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.ListSessions(ctx, appID, 200, 0)
		if err != nil {
			return pickerItemsMsg{err: fmt.Errorf("list sessions: %w", err)}
		}
		items := make([]PickerItem, 0, len(resp.Sessions))
		for _, sess := range resp.Sessions {
			// Label by topic : the first user message (daemon preview) is the most
			// meaningful ; fall back to an explicit title, then "(untitled)". The
			// generic "TUI chat" default counts as no title.
			name := sess.Preview
			if name == "" {
				name = sess.Title
			}
			if name == "" || name == "TUI chat" {
				name = "(untitled)"
			}
			label := name + "  " + shortID(sess.SessionID)
			if sess.SessionID == current {
				label = "● " + label // dot marker on the current session
			}
			// Relative age first (most useful at a glance), then the event count.
			hint := fmt.Sprintf("%d events", sess.EventCount)
			if rel := relTime(sess.UpdatedAt); rel != "" {
				hint = rel + " · " + hint
			}
			items = append(items, PickerItem{
				ID:    sess.SessionID,
				Label: label,
				Hint:  hint,
			})
		}
		return pickerItemsMsg{kind: "sessions", title: "sessions · " + appID, items: items, deletable: true}
	}
}

func (s *ChatScreen) loadAppsPicker() tea.Cmd {
	c := s.client
	currentApp := s.appID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		apps, err := c.ListApps(ctx, false)
		if err != nil {
			return pickerItemsMsg{err: fmt.Errorf("list apps: %w", err)}
		}
		items := make([]PickerItem, 0, len(apps))
		for _, a := range apps {
			label := a.Name
			if a.AppID == currentApp {
				label = "● " + label
			}
			items = append(items, PickerItem{
				ID:    a.AppID,
				Label: label,
				Hint:  a.AppID + " · " + a.Version,
			})
		}
		return pickerItemsMsg{kind: "apps", title: "installed apps", items: items}
	}
}

// abortTurn asks the daemon to interrupt the in-flight turn. The daemon
// replies with turn_ended status=interrupted, which the envelope handler
// already surfaces ; here we just fire the request + a toast.
func (s *ChatScreen) abortTurn() tea.Cmd {
	if !s.pendingTurn {
		return nil
	}
	c := s.client
	appID := s.appID
	sid := s.sessionID
	s.addToast(toastInfo, "interrupting…")
	return tea.Batch(
		func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return abortResultMsg{err: c.AbortTurn(ctx, appID, sid)}
		},
		s.ensureTick(),
	)
}

// copyMessage copies the selected message if one is highlighted, else
// falls back to the latest assistant reply.
func (s *ChatScreen) copyMessage() tea.Cmd {
	if content, ok := s.messages.SelectedContent(); ok {
		s.addToast(toastSuccess, "copied message to clipboard")
		return tea.Batch(tea.SetClipboard(content), s.ensureTick())
	}
	return s.copyLastAssistant()
}

// copyLastAssistant yanks the most recent assistant reply to the system
// clipboard via OSC52 (works over SSH).
func (s *ChatScreen) copyLastAssistant() tea.Cmd {
	msgs := s.messages.messages
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "assistant" && msgs[i].Content != "" {
			s.addToast(toastSuccess, "copied reply to clipboard")
			return tea.Batch(tea.SetClipboard(msgs[i].Content), s.ensureTick())
		}
	}
	s.addToast(toastWarning, "no assistant reply to copy")
	return s.ensureTick()
}

// OpenCommandPalette opens a fuzzy picker over every slash command —
// the opencode-style Ctrl+K palette. Returns nil (no-op) while a modal
// already owns the keyboard, so the key falls through to normal routing.
func (s *ChatScreen) OpenCommandPalette() tea.Cmd {
	if s.approval != nil || s.picker != nil {
		return nil
	}
	s.complActive = false
	s.complCursor = 0
	cmds := allSlashCommands
	return func() tea.Msg {
		items := make([]PickerItem, 0, len(cmds))
		for _, c := range cmds {
			items = append(items, PickerItem{ID: c.Name, Label: "/" + c.Name, Hint: c.Desc})
		}
		return pickerItemsMsg{kind: "commands", title: "commands", items: items}
	}
}

// OpenPicker is the global-keybinding entry to the session / app
// pickers (Ctrl+S / Ctrl+A). No-op while a modal is up.
func (s *ChatScreen) OpenPicker(kind string) tea.Cmd {
	if s.approval != nil || s.picker != nil {
		return nil
	}
	switch kind {
	case "sessions":
		return s.openSessionsPicker()
	case "apps":
		return s.openAppsPicker()
	}
	return nil
}

// openSessionsPicker / openAppsPicker show the picker immediately in a spinner
// state, then fetch the rows in the background — a session/app list can be slow
// to load (the daemon now reads each session's first message for the preview, so
// at thousands of sessions the round-trip is noticeable). pickerItemsMsg fills
// the loading picker in place when the data lands.
func (s *ChatScreen) openSessionsPicker() tea.Cmd {
	s.picker = NewLoadingPicker(s.theme, "sessions · "+s.appID)
	s.pickerKind = "sessions"
	return tea.Batch(s.loadSessionsPicker(), s.ensureTick())
}

func (s *ChatScreen) openAppsPicker() tea.Cmd {
	s.picker = NewLoadingPicker(s.theme, "apps")
	s.pickerKind = "apps"
	return tea.Batch(s.loadAppsPicker(), s.ensureTick())
}

// loadThemePicker opens the picker over the embedded opencode theme
// catalog. Synchronous (no network) — the names come from the in-memory
// registry — so it returns the pickerItemsMsg directly.
func (s *ChatScreen) loadThemePicker() tea.Cmd {
	current := s.theme.Name
	names := theme.Names()
	return func() tea.Msg {
		items := make([]PickerItem, 0, len(names))
		for _, n := range names {
			label := n
			if n == current {
				label = "● " + n
			}
			items = append(items, PickerItem{ID: n, Label: label, Hint: "theme"})
		}
		return pickerItemsMsg{kind: "themes", title: "themes", items: items}
	}
}

// applyTheme switches the live palette in place. Because every widget
// shares the one *theme.Theme pointer, mutating it re-colours the whole
// TUI ; we only have to force the markdown cache to re-render.
func (s *ChatScreen) applyTheme(name string) {
	if !s.theme.Apply(name) {
		s.addToast(toastWarning, "unknown theme: "+name)
		return
	}
	s.messages.Rebuild()
	theme.SavePreferred(name)
	s.addToast(toastSuccess, "theme → "+name)
}

// drillIntoChild switches the view to a sub-agent's isolated child session,
// remembering the parent to return to. The child's own events (its tool
// calls + reply) replay through the normal transcript.
func (s *ChatScreen) drillIntoChild(child string) tea.Cmd {
	s.drillParent = s.sessionID
	s.messages.ClearSelection()
	s.addToast(toastInfo, "viewing sub-agent — esc to go back")
	return tea.Batch(s.applySessionSwitch(child), s.ensureTick())
}

// popDrill returns from a sub-agent child session to its parent.
func (s *ChatScreen) popDrill() tea.Cmd {
	parent := s.drillParent
	s.drillParent = ""
	s.addToast(toastInfo, "back to main session")
	return tea.Batch(s.applySessionSwitch(parent), s.ensureTick())
}

// deleteSession removes a session over REST. The picker stays open and
// refreshes when sessionDeletedMsg lands ; deleting the active session
// triggers a fresh one so the chat never points at a dead session.
func (s *ChatScreen) deleteSession(sid string) tea.Cmd {
	c := s.client
	appID := s.appID
	wasCurrent := sid == s.sessionID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.DeleteSession(ctx, appID, sid); err != nil {
			return sessionDeletedMsg{err: err}
		}
		return sessionDeletedMsg{sid: sid, wasCurrent: wasCurrent}
	}
}

func (s *ChatScreen) newSession() tea.Cmd {
	c := s.client
	appID := s.appID
	reqWD := s.reqWorkdir
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		resp, err := c.CreateSession(ctx, appID, client.CreateSessionRequest{Workdir: reqWD})
		if err != nil {
			return switchSessionMsg{err: err}
		}
		wd := resp.Workdir
		if wd == "" {
			wd = resp.Workspace
		}
		return switchSessionMsg{sessionID: resp.SessionID, workdir: wd}
	}
}

func (s *ChatScreen) showHelp() tea.Cmd {
	s.help = true
	return nil
}

// appendInlineSystem injects a synthetic system message into the
// viewport — useful for slash-command output, errors, and tips.
// resolveApproval answers the active approval over REST. The daemon
// signals its runtime registry (unblocking the suspended turn) and
// emits approval_granted/denied, which clears the modal. Snapshots the
// ids up front since s.approval may be cleared by an envelope before
// the command runs.
func (s *ChatScreen) resolveApproval(action string) tea.Cmd {
	if s.approval == nil {
		return nil
	}
	c := s.client
	appID := s.appID
	sessionID := s.sessionID
	approvalID := s.approval.ID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return approvalResolvedMsg{err: c.ResolveApproval(ctx, appID, sessionID, approvalID, action, "")}
	}
}

// resolveAsk answers an ask_user question. The assembled reply rides in the
// approval `reason` field — the daemon's bridge returns it verbatim to the
// agent (Reason carries the answer for both approve and deny).
func (s *ChatScreen) resolveAsk(action, answer string) tea.Cmd {
	if s.askForm == nil {
		return nil
	}
	c := s.client
	appID := s.appID
	sessionID := s.sessionID
	approvalID := s.askForm.ID()
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return approvalResolvedMsg{err: c.ResolveApproval(ctx, appID, sessionID, approvalID, action, answer)}
	}
}

// appendInlineSystem surfaces a muted system note in the transcript
// (slash-command output, tips). Thin wrapper over the shared injection
// chokepoint so it inherits correct seq anchoring.
func (s *ChatScreen) appendInlineSystem(text string) {
	s.injectMessage("system", text)
}

// handlePickerSelection routes the picker's terminal choice to the
// appropriate action handler.
func (s *ChatScreen) handlePickerSelection(kind, selectedID string) tea.Cmd {
	switch kind {
	case "sessions":
		if selectedID == s.sessionID {
			return nil // no-op
		}
		return func() tea.Msg { return switchSessionMsg{sessionID: selectedID} }
	case "apps":
		// App switch = quit & relaunch isn't great. Instead : update
		// appID inline and start a fresh session under it.
		if selectedID == s.appID {
			return nil
		}
		return s.switchApp(selectedID)
	case "themes":
		s.applyTheme(selectedID)
		return s.ensureTick()
	case "commands":
		// Run the chosen command exactly as if it had been typed.
		return s.dispatchCommand("/" + selectedID)
	}
	return nil
}

// switchApp swaps the current app, fetches the app's display name,
// then creates a fresh session under it.
func (s *ChatScreen) switchApp(appID string) tea.Cmd {
	// Swap appID up front so FetchModel (batched below) captures the new
	// one ; clear the stale model until the manifest fetch returns.
	s.appID = appID
	s.model = ""
	// Drop the previous app's modes ; FetchModes (batched) reloads the new app's.
	s.modes = nil
	s.modeIdx = 0
	s.input.SetMode("")
	c := s.client
	return tea.Batch(
		func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			app, err := c.GetApp(ctx, appID)
			if err != nil {
				return switchSessionMsg{err: fmt.Errorf("get app %s: %w", appID, err)}
			}
			s.appName = app.Name
			return s.newSession()()
		},
		s.FetchModel(),
		s.FetchModes(),
		s.FetchAppInfo(),
	)
}

// applySessionSwitch tears down the current realtime stream, swaps the
// sessionID, clears the message viewport, and reconnects. Everything
// post-swap flows through the new session's replay since=0.
//
// NB : we no longer try to drain envCh before reconnecting — the drain
// goroutine raced against the new session's replay events and would
// swallow them. Instead we let any stale buffered events flow through
// and rely on handleEnvelope's session_id filter to ignore them.
func (s *ChatScreen) applySessionSwitch(newSessionID string) tea.Cmd {
	if s.rt != nil {
		_ = s.rt.Close()
		s.rt = nil
	}
	if s.connStop != nil {
		close(s.connStop)
		s.connStop = nil
	}
	s.sessionID = newSessionID
	s.pendingTurn = false
	s.currentPhase = ""
	s.subActivity = nil // activity belongs to the session we left
	// Agent-correlation maps are per-session : clearing them stops a stale
	// call_id/run_id from the old session binding a new spawn to a dead chip,
	// or a drill-in landing on a child session that belonged to the old app.
	// ensureAgentMaps re-creates them lazily on the next event.
	s.agentCalls = nil
	s.agentRunCall = nil
	s.agentChild = nil
	s.agentRunChild = nil
	s.agentRunKind = nil
	s.agentToolCount = nil
	s.lastAgentCallByKind = nil
	s.parallelArgs = nil
	s.seenSeq = nil // fresh dedup window for the new session's seq space
	s.todos = nil
	s.pendingApproval = ""
	s.sendQueue = nil // queued messages belong to the session we just left
	if s.askForm != nil || s.approval != nil {
		s.askForm = nil // a pending prompt belongs to the session we left
		s.approval = nil
		s.input.Focus()
	}
	s.messages.SetMessages(nil)
	s.loadingSession = true // hide the transcript until the backlog finishes replaying
	// Only (re)connect : do NOT issue a fresh waitForEnvelope. The listener from
	// the previous session is still blocked on the shared envCh (it's re-armed by
	// each connStateMsg/envelopeMsg, never retired), so adding one here would leak
	// a second listener that splits events — one more per switch.
	return tea.Batch(s.connectRealtime(), s.scheduleSessionLoadTimeout(), s.ensureTick())
}

// SessionID exposes the active session UUID for the status bar.
func (s *ChatScreen) SessionID() string { return s.sessionID }

// AppName exposes the active app's display name for the sidebar header.
func (s *ChatScreen) AppName() string { return s.appName }

// PendingTurn exposes the turn-in-flight state for the status bar.
func (s *ChatScreen) PendingTurn() bool { return s.pendingTurn }

// refreshCompletion re-evaluates whether the slash-command popup should
// be visible based on the current input value, and filters the list.
// Called after every keystroke routed to the textarea.
func (s *ChatScreen) refreshCompletion() {
	val := s.input.Value()
	if !strings.HasPrefix(val, "/") {
		s.complActive = false
		s.complCursor = 0
		return
	}
	// Strip the leading "/", trim trailing whitespace+args. We filter on
	// the first word only — once the user types a space, they're past
	// the command name and the popup hides.
	rest := strings.TrimPrefix(val, "/")
	if strings.Contains(rest, " ") || strings.Contains(rest, "\n") {
		s.complActive = false
		s.complCursor = 0
		return
	}
	prefix := strings.ToLower(rest)
	s.complFiltered = s.complFiltered[:0]
	for _, c := range allSlashCommands {
		if strings.HasPrefix(strings.ToLower(c.Name), prefix) {
			s.complFiltered = append(s.complFiltered, c)
		}
	}
	if len(s.complFiltered) == 0 {
		s.complActive = false
		s.complCursor = 0
		return
	}
	if s.complCursor >= len(s.complFiltered) {
		s.complCursor = 0
	}
	s.complActive = true
}

// replaceInputWith resets the textarea contents to a new string. Used
// by Tab completion to finish a partial command name.
func (s *ChatScreen) replaceInputWith(text string) {
	s.input.ta.Reset()
	s.input.ta.InsertString(text)
}

// renderCompletionPopup draws the small dropdown shown just above the
// input while the user is typing a slash command.
func (s *ChatScreen) renderCompletionPopup(width int) string {
	if !s.complActive || len(s.complFiltered) == 0 {
		return ""
	}
	rowStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.Text))
	selStyle := lipgloss.NewStyle().
		Foreground(lipgloss.Color(s.theme.Background)).
		Background(lipgloss.Color(s.theme.Primary)).
		Bold(true)
	muted := lipgloss.NewStyle().Foreground(lipgloss.Color(s.theme.TextMuted))

	var rows []string
	for i, c := range s.complFiltered {
		name := "/" + c.Name
		desc := muted.Render("— " + c.Desc)
		// Build the row text manually so selection painting spans the
		// full popup width.
		raw := " " + name + "  " + desc + " "
		// Strip ANSI to measure visible width
		visible := lipgloss.Width(raw)
		if visible < width-2 {
			raw += strings.Repeat(" ", (width-2)-visible)
		}
		if i == s.complCursor {
			rows = append(rows, selStyle.Render(" "+name+"  "+desc+strings.Repeat(" ", max(0, (width-2)-lipgloss.Width(" "+name+"  "+desc)))+" "))
		} else {
			rows = append(rows, rowStyle.Render(raw))
		}
	}
	hint := muted.Render(" ↑↓ navigate · tab complete · enter run · esc cancel ")

	rows = append(rows, hint)
	body := lipgloss.JoinVertical(lipgloss.Left, rows...)
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(s.theme.BorderActive)).
		Width(width - 2).
		Render(body)
}

// max is a tiny stdlib-shim helper (Go 1.21+ has builtin max, kept here
// for older minor versions of toolchain).
func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ConnState exposes the realtime connection state for the status bar. It reads
// the LIVE socket state from the realtime client rather than the cached
// s.connState : the cached value rides the same buffered envelope channel as
// token deltas (drop-oldest under load), so it can lag or miss a transition and
// leave the connection dot out of sync with reality. s.rt.State() is the
// authoritative current state and is concurrency-safe. Falls back to the cached
// value before the client is wired (s.rt nil during connect / session switch).
func (s *ChatScreen) ConnState() client.ConnState {
	if s.rt != nil {
		return s.rt.State()
	}
	return s.connState
}
