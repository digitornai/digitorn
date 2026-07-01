package tui

import (
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/digitornai/digitorn-cli/internal/client"
	"github.com/digitornai/digitorn-cli/internal/render"
	"github.com/digitornai/digitorn-cli/internal/theme"
)

// Messages renders the chat scrollback. Chat-style layout :
//   - user bubbles : right-aligned, primary-tinted border, plain text
//   - assistant    : left-aligned, glamour-rendered markdown, no chrome
//   - system       : centered separator, muted italic
//   - error        : full-width red bordered panel
//   - tool         : left-aligned panel with muted text
//
// No role labels ("you"/"assistant") — position + colour identify the
// speaker, same convention as WhatsApp / iMessage.
type Messages struct {
	theme    *theme.Theme
	vp       viewport.Model
	messages []client.Message
	width    int

	// committed caches the rendered scrollback (markdown included) so a
	// streamed token only re-paints the cheap transient bubble, never
	// re-runs glamour over the whole history.
	committed string

	// renderCache memoizes each message's fully-rendered block (markdown /
	// diff / chip, selection gutter included) keyed by everything that
	// affects its output. A rebuild reuses unchanged blocks so only the
	// message that actually changed re-runs glamour — without it, every
	// tool-chip update during a turn re-rendered the WHOLE transcript, a cost
	// that grew with session length. Pruned to the live transcript each pass
	// (see rebuild), so it never outgrows the messages on screen. Running
	// chips depend on wall-clock elapsed time and are deliberately not cached.
	renderCache map[string]string

	// Tool results are collapsed by default ; expanded holds the call IDs
	// the user has clicked open. chipAt maps each committed content-line
	// index to the call ID of the tool chip occupying it, so a mouse click
	// (translated through the viewport scroll offset) can hit-test which
	// chip to toggle.
	expanded map[string]bool
	chipAt   map[int]string

	// Message selection (keyboard navigation + copy). selIdx is the index
	// of the highlighted message, or -1 for none. msgStart maps each
	// message to the committed content-line it begins at, to scroll the
	// selection into view.
	selIdx   int
	msgStart []int
	// stream holds the in-progress assistant reply during token streaming. It's
	// markdown-rendered live (like the committed message), but the render is
	// coalesced to one per animation tick — streamDirty marks a pending repaint
	// so a burst of tokens costs one render per frame, not one per token.
	// detached is the sticky auto-scroll flag : false (default) = pinned to the
	// bottom, so EVERY content change (new message, tool chip, a running chip's
	// live duration, streamed token) re-pins the view to the latest line. It
	// flips true ONLY when the USER scrolls away from the bottom (tracked in
	// Update), and back to false when they scroll back down. Deriving "follow?"
	// from a fresh AtBottom() on every update was racy : one update that grew the
	// content without re-pinning dropped the view off the bottom, and from then
	// on AtBottom() stayed false so NOTHING re-pinned until the turn ended — which
	// is exactly why a long-running tool was only revealed at the very end.
	stream      string
	streamDirty bool
	detached    bool
	// streamTools holds pre-rendered lines for tool calls the model is STILL
	// emitting (name + a growing token counter). They live in the live bottom
	// area, BELOW the committed timeline and the streaming text — never as
	// seq'd messages, so they can't sort ahead of the assistant's intent. The
	// real, ordered chip materialises from the durable pending/result event.
	streamTools string
	// think holds the agent's in-progress thinking (reasoning) trace, streamed
	// live and rendered dimmed ABOVE the streaming answer. Cleared when the
	// message commits ; the consolidated reasoning then rides the message.
	think string
	// streamThrottle counts animation ticks since the last live markdown render of
	// the in-progress reply. Re-glamour-ing the WHOLE buffer every 120ms tick gets
	// quadratically slower as the reply grows (O(n) render × O(n) ticks), so we
	// repaint the stream only every streamRenderEveryTicks ticks. The final commit
	// still renders immediately.
	streamThrottle int

	// Streaming glamour render cache. Re-glamouring the whole growing reply is
	// O(n) and dominates the loop on long answers, so renderStreaming reuses the
	// last output until an ADAPTIVE cooldown elapses — the cooldown scales with
	// how long the render itself took (streamDur), capping render cost at a small
	// fraction of wall-clock no matter how long the reply gets. Chips can still
	// repaint every tick (they reuse streamRendered, no re-glamour).
	streamRendered string        // cached glamour output of the in-progress reply
	streamRenderAt time.Time     // when it was last computed
	streamDur      time.Duration // how long that last glamour render took
	streamWidth    int           // width the cache was rendered at (resize invalidates)

	// spinFrame advances once per animation tick (RefreshRunning) so an
	// in-progress tool/agent chip shows a moving braille spinner instead of a
	// dead static glyph.
	spinFrame int
}

// spinnerFrames is the braille sweep used for running chips (opencode's set).
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// SpinFrame exposes the current animation frame so other widgets (the sidebar
// activity panel) animate their running glyphs in lockstep with the chat chips.
func (m *Messages) SpinFrame() int { return m.spinFrame }

func spinnerGlyph(frame int) string {
	return spinnerFrames[((frame%len(spinnerFrames))+len(spinnerFrames))%len(spinnerFrames)]
}

func NewMessages(t *theme.Theme) *Messages {
	vp := viewport.New()
	vp.SetWidth(80)
	vp.SetHeight(20)
	// Always render to the configured height, padding short content with
	// empty rows. Otherwise the chat body is shorter than the chat-screen
	// height when there are few messages, the sidebar gets visually
	// clipped at the body's last row, and its thick left border appears
	// to "stop short" of the bottom edge.
	vp.FillHeight = true
	return &Messages{theme: t, vp: vp, expanded: map[string]bool{}, chipAt: map[int]string{}, selIdx: -1}
}

func (m *Messages) SetSize(w, h int) {
	if w < 20 {
		w = 20
	}
	if h < 3 {
		h = 3
	}
	m.width = w
	m.vp.SetWidth(w)
	m.vp.SetHeight(h)
	m.rebuild()
}

func (m *Messages) SetMessages(msgs []client.Message) {
	if len(msgs) == len(m.messages) && messagesEqual(msgs, m.messages) {
		return
	}
	m.messages = append([]client.Message(nil), msgs...)
	m.selIdx = -1 // a changed transcript invalidates the selection
	m.rebuild()
	if !m.detached {
		m.vp.GotoBottom()
	}
}

// Rebuild forces a full re-render of the scrollback (markdown included).
// Called after a live theme switch so the committed cache picks up the
// new palette ; the per-message cache is dropped because its keys don't
// include the theme (otherwise it holds output coloured by the old theme).
func (m *Messages) Rebuild() {
	m.renderCache = nil
	m.rebuild()
}

// isChipRole reports whether a message renders as an inline tool/agent chip
// (vs prose). Used to put a blank line at prose↔chip-group boundaries.
func isChipRole(role string) bool { return role == "tool" || role == "agent" }

func (m *Messages) rebuild() {
	if m.width == 0 {
		return
	}
	contentWidth := m.contentWidth()
	m.chipAt = map[int]string{}
	m.msgStart = m.msgStart[:0]
	// Reuse last pass's blocks ; collect this pass's into `next` so the cache
	// is pruned to exactly the messages still on screen (bounded memory).
	prev := m.renderCache
	next := make(map[string]string, len(m.messages))
	var b strings.Builder
	line := 0
	for i, msg := range m.messages {
		// One blank line between consecutive messages for an even rhythm —
		// EXCEPT between two tool/agent chips, which stay tight so a run of tool
		// calls reads as one group. Counted as a line so click/selection
		// hit-testing stays aligned ; outside the cached block so it re-evaluates.
		if i > 0 && !(isChipRole(msg.Role) && isChipRole(m.messages[i-1].Role)) {
			b.WriteByte('\n')
			line++
		}
		m.msgStart = append(m.msgStart, line)
		// Each renderXxx returns its block + one trailing newline.
		sel := i == m.selIdx
		key, cacheable := blockCacheKey(msg, contentWidth, sel, m.expanded[msg.CallID])
		var block string
		if cacheable {
			if hit, ok := prev[key]; ok {
				block = hit
			} else {
				block = m.produceBlock(msg, contentWidth, sel)
			}
			next[key] = block
		} else {
			block = m.produceBlock(msg, contentWidth, sel)
		}
		if (msg.Role == "tool" || msg.Role == "agent") && msg.CallID != "" {
			// Map every line of this chip to its call ID so a click
			// anywhere on it toggles the right result.
			for j := 0; j < strings.Count(block, "\n"); j++ {
				m.chipAt[line+j] = msg.CallID
			}
		}
		line += strings.Count(block, "\n")
		b.WriteString(block)
	}
	m.committed = b.String()
	m.renderCache = next
	m.refresh()
}

// produceBlock renders one message's final block : the per-role render at the
// selection-adjusted width, with the selection gutter applied. The pure
// rendering half of the rebuild loop, split out so the cache can wrap it.
func (m *Messages) produceBlock(msg client.Message, contentWidth int, sel bool) string {
	bw := contentWidth
	if sel {
		// Reserve 2 cols for the selection gutter so total width holds.
		if bw -= 2; bw < 12 {
			bw = 12
		}
	}
	block := m.renderOne(msg, bw)
	if sel {
		block = m.selGutter(block)
	}
	return block
}

// blockCacheKey hashes everything that affects a message's rendered block. The
// second return is false for a block that must never be cached — a running
// tool/agent chip, whose rendered duration depends on the wall clock.
func blockCacheKey(msg client.Message, width int, sel, expanded bool) (string, bool) {
	if (msg.Role == "tool" || msg.Role == "agent") && !terminalChipStatus(msg.Status) {
		return "", false
	}
	h := fnv.New64a()
	writeKey := func(parts ...string) {
		for _, p := range parts {
			_, _ = h.Write([]byte(p))
			_, _ = h.Write([]byte{0})
		}
	}
	writeKey(
		msg.Role, msg.Content, msg.Status, msg.CallID,
		msg.ToolArg, msg.ToolOutput, msg.ToolDiff,
		strconv.FormatInt(msg.DurationMs, 10),
		strconv.Itoa(width),
		strconv.FormatBool(sel), strconv.FormatBool(expanded),
	)
	return strconv.FormatUint(h.Sum64(), 36), true
}

// terminalChipStatus reports whether a tool/agent chip has reached a final
// state — i.e. its render no longer depends on elapsed time and is safe to
// cache. Mirrors HasRunning's terminal set.
func terminalChipStatus(status string) bool {
	switch status {
	case "completed", "done", "ok", "success", "errored", "error", "failed", "cancelled", "ended":
		return true
	default:
		return false
	}
}

func (m *Messages) contentWidth() int {
	w := m.width - 2
	if w < 20 {
		w = 20
	}
	return w
}

// refresh re-paints the viewport from the cached committed render plus
// the transient streaming bubble. Cheap (no markdown re-render of the
// scrollback), so it's safe to call on every streamed token.
func (m *Messages) refresh() {
	if m.width == 0 {
		return
	}
	content := m.committed
	if m.think != "" {
		content += m.renderThinking(m.think)
	}
	if m.stream != "" {
		content += m.renderStreaming(m.stream, m.contentWidth())
	}
	if m.streamTools != "" {
		content += m.streamTools
	}
	m.vp.SetContent(content)
}

// SetThinking sets the live in-progress thinking trace. Pass "" to clear it
// (when the message commits). Rendered dimmed above the streaming answer.
func (m *Messages) SetThinking(text string) {
	if text == m.think {
		return
	}
	m.think = text
	m.refresh()
	if !m.detached {
		m.vp.GotoBottom()
	}
}

// renderThinking paints the live thinking phase TOTALLY MINIMIZED : a single
// dimmed 💭 Thinking… indicator, NO reasoning prose at all. The full trace is
// still captured on the committed message (and stays collapsed there too).
func (m *Messages) renderThinking(text string) string {
	if strings.Trim(text, "\n") == "" {
		return ""
	}
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.TextMuted)).Faint(true)
	return "\n" + faint.Render("💭 Thinking…") + "\n"
}

// SetStreamingTools sets the live bottom block of in-progress tool calls
// (already rendered to lines). Pass "" to clear. Rendered below the committed
// timeline and the streaming text, so a tool being emitted never sorts ahead of
// the assistant message that announces it.
func (m *Messages) SetStreamingTools(s string) {
	if s == m.streamTools {
		return
	}
	m.streamTools = s
	m.refresh()
	if !m.detached {
		m.vp.GotoBottom()
	}
}

// SetStreaming updates the in-progress assistant bubble. Pass "" to
// clear it (e.g. when the final assistant_message commits). Keeps the
// view pinned to the bottom so the growing reply stays visible.
func (m *Messages) SetStreaming(text string) {
	if text == m.stream {
		return
	}
	m.stream = text
	if text == "" {
		// Clearing (the final message just committed) : paint immediately so the
		// bubble disappears without a one-frame flicker. Drop the render cache so
		// the next reply starts fresh.
		m.streamDirty = false
		m.streamThrottle = 0
		m.streamRendered = ""
		m.streamDur = 0
		m.refresh()
		if !m.detached {
			m.vp.GotoBottom()
		}
		return
	}
	// Otherwise defer the markdown render to the next animation tick : a burst
	// of tokens then coalesces into a single render per frame (see RefreshRunning).
	m.streamDirty = true
}

// renderStreaming paints the in-progress reply through the SAME glamour
// renderer as a committed reply (opencode's approach : always render the full
// partial buffer, never raw source). The earlier garble was not glamour itself
// — it was agentBlock's post-render "\x1b[0m"→bg surgery corrupting partial
// ANSI ; now the background is baked into the glamour style (render.Markdown's
// bg arg) so there is nothing to patch, and partial documents render cleanly.
// Rendering the whole buffer (not appending fragments) means an unclosed fence
// or half table just renders as-is and self-corrects as more tokens land. The
// per-tick cost is bounded by the throttle in RefreshRunning + the cached
// renderer, and there is no jarring raw-source-then-snap transition on commit.
func (m *Messages) renderStreaming(text string, width int) string {
	// Adaptive throttle : reuse the last glamour render until a cooldown elapses.
	// The cooldown is max(minStreamCooldown, streamCostFactor × last render time),
	// capped at maxStreamCooldown — so a short reply repaints often (cheap) while
	// a long one (slow glamour) repaints less, never letting the render eat more
	// than ~1/streamCostFactor of wall-clock. The buffer keeps filling regardless;
	// only the VISIBLE refresh is paced. The final, full render lands on commit.
	if m.streamRendered != "" && width == m.streamWidth {
		cooldown := minStreamCooldown
		if c := streamCostFactor * m.streamDur; c > cooldown {
			cooldown = c
		}
		if cooldown > maxStreamCooldown {
			cooldown = maxStreamCooldown
		}
		if time.Since(m.streamRenderAt) < cooldown {
			return m.streamRendered // reuse — no re-glamour this frame
		}
	}
	start := time.Now()
	body := strings.Trim(render.Markdown(strings.Trim(text, "\n"), width-6, m.theme, agentReplyBg), "\n")
	m.streamDur = time.Since(start)
	// Same left-bar block as a committed reply. No trailing cursor : the working
	// indicator at the bottom already signals live generation.
	out := "\n" + m.agentBlock(body) + "\n"
	m.streamRendered = out
	m.streamRenderAt = start
	m.streamWidth = width
	return out
}

func (m *Messages) renderOne(msg client.Message, width int) string {
	switch msg.Role {
	case "user":
		return m.renderUser(msg, width)
	case "assistant":
		return m.renderAssistant(msg, width)
	case "system":
		return m.renderSystem(msg, width)
	case "tool":
		return m.renderTool(msg, width)
	case "agent":
		return m.renderAgent(msg, width)
	case "todo":
		return m.renderTodo(msg, width)
	case "compaction":
		return m.renderCompaction(msg, width)
	case "error":
		return m.renderError(msg, width)
	}
	// Unknown role : muted plain text, no alignment.
	return lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.TextMuted)).Render(msg.Content) + "\n"
}

func (m *Messages) renderUser(msg client.Message, _ int) string {
	// User message : a FULL-WIDTH panel band (background spans the whole row),
	// but the TEXT is pushed to the RIGHT inside it, with the accent bar at the
	// right edge. Assistant text + tool chips stay on the left.
	// Full-width panel band : the background covers the whole row out to the
	// LEFT edge, the text is right-aligned inside it, the accent bar sits at the
	// right edge. NB lipgloss Width here is the TOTAL box width (padding +
	// border included), so it's the full viewport width — not width-5, which
	// left ~5 unpainted columns on the right.
	w := m.width
	if w < 16 {
		w = 16
	}
	return lipgloss.NewStyle().
		Background(lipgloss.Color(m.theme.BackgroundPanel)).
		Foreground(lipgloss.Color(m.theme.Text)).
		Border(lipgloss.ThickBorder(), false, true, false, false).
		BorderForeground(lipgloss.Color(m.theme.Secondary)).
		BorderBackground(lipgloss.Color(m.theme.BackgroundPanel)).
		Padding(1, 2).
		Width(w).
		Align(lipgloss.Right).
		Render(msg.Content) + "\n"
}

func (m *Messages) renderAssistant(msg client.Message, width int) string {
	body := strings.Trim(render.Markdown(msg.Content, width-6, m.theme, agentReplyBg), "\n")
	out := m.agentBlock(body) + "\n"
	// Collapsed thinking block : when the agent reasoned before answering, show
	// a dimmed 💭 teaser above the reply (the full trace streamed live).
	if msg.Reasoning != "" {
		out = m.renderThoughtCollapsed() + out
	}
	return out
}

// renderThoughtCollapsed renders the persisted reasoning TOTALLY MINIMIZED : a
// dimmed 💭 thought marker with NO prose. The full reasoning stays on the
// message (Reasoning field) but is never painted as text.
func (m *Messages) renderThoughtCollapsed() string {
	faint := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.TextMuted)).Faint(true)
	return faint.Render("  💭 thought") + "\n"
}

// agentBlock frames the assistant's reply as a FULL-WIDTH panel band — the
// mirror of the user message (which is a right-aligned band) : a left accent bar
// + a BackgroundPanel fill spanning the whole row, left-aligned text.
// agentReplyBg is the subtle fill behind an assistant reply : darker than the
// user message's BackgroundPanel (#141414) — close to the terminal so it reads
// as a faint surface, not a loud band. Tweak this one value to taste.
const agentReplyBg = "#101010"

func (m *Messages) agentBlock(body string) string {
	w := m.width
	if w < 16 {
		w = 16
	}
	// No ANSI surgery : the background is baked INTO the glamour render
	// (render.Markdown bg arg) and glamour cascades it to every element, so the
	// fill is already unbroken behind the text. The band's own Background only
	// needs to cover the border + padding cells around it.
	return lipgloss.NewStyle().
		Background(lipgloss.Color(agentReplyBg)).
		Foreground(lipgloss.Color(m.theme.Text)).
		Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color(m.theme.Primary)).
		BorderBackground(lipgloss.Color(agentReplyBg)).
		Padding(1, 2).
		Width(w).
		Render(body)
}

func (m *Messages) renderSystem(msg client.Message, width int) string {
	style := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.TextMuted)).
		Italic(true)
	// Keep the centred one-liner on a single row : truncate so a long system
	// note can't spill past the viewport edge.
	text := style.Render(truncate("— "+msg.Content+" —", width))
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, text) + "\n"
}

// renderCompaction draws the end-of-compaction note : a subtle full-width line
// with the freed-tokens summary on the LEFT (a "⟢" lead glyph, secondary tint)
// and the post-compaction "ctx used/window" gauge on the RIGHT (muted). No
// panel / bar / bubble — it reads as a quiet divider in the flow, distinct from
// a tool chip, marking where the context shrank.
func (m *Messages) renderCompaction(msg client.Message, width int) string {
	left := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.Secondary)).Faint(true).
		Render("⟢ " + msg.Content)
	right := ""
	if msg.ToolArg != "" {
		right = lipgloss.NewStyle().
			Foreground(lipgloss.Color(m.theme.TextMuted)).Faint(true).
			Render(msg.ToolArg)
	}
	gap := width - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right + "\n"
}

// toolVerb maps a tool's action to a human label for the chip header
// (opencode style) : a shell run reads "$ <cmd>", a write "Wrote <file>", a
// grep "Grep <pattern>". Unknown tools keep their raw action name.
func toolVerb(action string) string {
	switch action {
	case "run", "bash", "exec", "shell":
		return "$"
	case "write":
		return "Wrote"
	case "edit", "multi_edit":
		return "Edit"
	case "read":
		return "Read"
	case "glob":
		return "Search"
	case "grep":
		return "Grep"
	case "search":
		// web.search / index.search / vector.search / rag.search — a query
		// lookup, NOT a filesystem grep. (filesystem's tool is "grep".)
		return "Search"
	case "fetch":
		return "Fetch"
	case "ls", "list":
		return "List"
	case "background_run":
		return "Background"
	case "run_parallel":
		// No verb word — the argument carries "N actions · names", which is
		// what's meaningful to a user (not the "parallel" implementation detail).
		return ""
	case "agent":
		return "Agent"
	default:
		return action
	}
}

// renderTool draws an inline tool-call chip : a state-coloured icon, the
// action, a muted argument hint, and a status/duration suffix — plus, for
// a completed call with output, a dimmed left-bordered preview block.
// Updates in place (same CallID) when the tool_result arrives.
//
//	⚙ read seed.txt · running…
//	✓ read seed.txt · 3ms
//	│ the magic number is 4242
//	✗ write poc.txt · failed
func (m *Messages) renderTool(msg client.Message, width int) string {
	// Running : an animated braille spinner in the interactive primary colour
	// (re-rendered each tick via RefreshRunning). Terminal states override it.
	icon, col := spinnerGlyph(m.spinFrame), m.theme.Primary
	suffix := runningSuffix(msg.TsUnixNano)
	switch msg.Status {
	case "awaiting", "awaiting_approval":
		// Gated behind an approval : a STATIC paused glyph (not a spinner) — it
		// isn't running, it's waiting on the user. Flips to "running" once granted.
		icon, col = "⏸", m.theme.Warning
		suffix = " · awaiting approval"
	case "completed", "done", "ok", "success":
		icon, col = "✓", m.theme.Success
		if d := formatDuration(msg.DurationMs); d != "" {
			suffix = " · " + d
		} else {
			suffix = ""
		}
	case "errored", "error", "failed":
		icon, col = "✗", m.theme.Error
		suffix = " · failed"
	case "ended":
		icon, col = "·", m.theme.TextMuted
		suffix = " · ended"
	}
	action := stripModulePrefix(msg.Content)
	// Expandable whenever there's detail to show — including the error text
	// on a failed call, so the user can click to see why it failed. A file
	// mutation (edit/write) carries a unified diff : we prefer it over the
	// plain output and render a coloured opencode-style preview.
	hasDiff := msg.ToolDiff != ""
	hasOutput := hasDiff || msg.ToolOutput != ""
	expanded := m.expanded[msg.CallID]
	diffStat := ""
	if hasDiff {
		diffStat = render.DiffStat(msg.ToolDiff)
	}

	// The icon + tool name keep the status colour (green ✓ / red ✗ / muted)
	// as before. The argument (a file name) is plain Text — NOT a brand colour
	// like Secondary, which several themes also map to a syntax token (e.g.
	// aura: secondary == syntaxKeyword == pink), making the file name in the
	// header collide with keywords in the diff body. Duration/line-count stay
	// faint, the caret in the interactive primary colour.
	// EVERY header segment carries the chip's panel background. Without it the
	// per-segment ANSI resets punch holes in the panel tint, so the text (the
	// argument especially) renders on the terminal background — looking like it
	// has a different/missing background inside the block.
	base := lipgloss.NewStyle().Background(lipgloss.Color(m.theme.BackgroundPanel))
	muted := base.Foreground(lipgloss.Color(m.theme.TextMuted))
	// The argument (file / command / pattern) gets a SUBTLE accent tint —
	// the secondary colour, softened with faint — so it reads as the call's
	// subject without shouting.
	argStyle := base.Foreground(lipgloss.Color(m.theme.Secondary)).Faint(true)
	// Terminals can't shrink the font (fixed monospace grid) — the standard
	// way to make text read "smaller" is the faint/dim attribute. Duration
	// and line-count use it so they recede next to the name + argument.
	metaStyle := muted.Faint(true)
	linesStyle := metaStyle.Italic(true)
	caretStyle := base.Foreground(lipgloss.Color(m.theme.Primary))

	head := icon
	if v := toolVerb(action); v != "" {
		head += " " + v
	}
	// Collapsed hint + expand caret, built first so the argument can be budgeted
	// to the space that's left. The hint is the diff stat for a file mutation
	// (+A -D) ; a line count ONLY for `read` (where "N lines" describes the file)
	// — for other tools a raw output line count is noise, so they show just the
	// caret.
	hint := ""
	if hasOutput && !expanded {
		switch {
		case hasDiff && diffStat != "":
			hint = diffStat
		case action == "read":
			hint = outputLineHint(msg.ToolOutput)
		}
	}
	caret := ""
	if hasOutput {
		caret = " ▸"
		if expanded {
			caret = " ▾"
		}
	}
	// The argument (file / command / pattern) is the only variable-width part :
	// truncate it with "…" so the header never overflows the chip. The full text
	// is one click away in the expanded body.
	arg := msg.ToolArg
	if arg != "" {
		hintW := 0
		if hint != "" {
			hintW = lipgloss.Width("  · " + hint)
		}
		budget := (m.width - 3) - lipgloss.Width(head) - 1 - lipgloss.Width(suffix) - hintW - lipgloss.Width(caret)
		if budget < 1 {
			arg = ""
		} else if lipgloss.Width(arg) > budget {
			arg = truncate(arg, budget)
		}
	}

	styled := base.Foreground(lipgloss.Color(col)).Render(head)
	if arg != "" {
		styled += argStyle.Render(" " + arg)
	}
	styled += metaStyle.Render(suffix)
	if hint != "" {
		styled += metaStyle.Render("  · ") + linesStyle.Render(hint)
	}
	if caret != "" {
		styled += caretStyle.Render(caret)
	}
	out := styled
	if hasOutput && expanded {
		if hasDiff {
			out += "\n" + m.renderDiffPreview(msg.ToolDiff, width)
		} else {
			out += "\n" + m.renderToolPreview(msg.ToolOutput)
		}
	}
	// Contain the whole tool call in a tinted panel block, its left bar tinted
	// by the call's status colour (green ok / red fail / running) so it reads
	// apart from the user (Secondary bar) and agent (grey bar) blocks.
	return m.chipBlock(out, col) + "\n"
}

// renderAgent draws a sub-agent run as an inline chip — same collapse /
// click-to-expand behaviour as a tool chip, but with a diamond icon, an
// "agent <kind>" label, telemetry as the muted hint, and the result
// summary (prose, not code) as the expandable body.
func (m *Messages) renderAgent(msg client.Message, width int) string {
	// Running sub-agent : the same animated spinner as a tool chip, in the
	// accent colour. Terminal states override it below.
	icon, col := spinnerGlyph(m.spinFrame), m.theme.Accent
	suffix := runningSuffix(msg.TsUnixNano)
	switch msg.Status {
	case "completed", "done", "ok", "success":
		icon, col = "✓", m.theme.Success
		if d := formatDuration(msg.DurationMs); d != "" {
			suffix = " · " + d
		} else {
			suffix = ""
		}
	case "errored", "error", "failed", "cancelled":
		icon, col = "✗", m.theme.Error
		st := msg.Status
		if st == "" {
			st = "failed"
		}
		suffix = " · " + st
	case "ended":
		icon, col = "◇", m.theme.TextMuted
		suffix = " · ended"
	}
	// Live count of tools the sub-agent has run (from its fanned tool_calls).
	if msg.ToolCount > 0 {
		suffix += fmt.Sprintf(" · %d tools", msg.ToolCount)
	}
	// Every segment carries the chip's panel background (see renderTool) so the
	// tint paints behind the text, not just the padding.
	base := lipgloss.NewStyle().Background(lipgloss.Color(m.theme.BackgroundPanel))
	muted := base.Foreground(lipgloss.Color(m.theme.TextMuted))
	metaStyle := muted.Faint(true)
	linesStyle := metaStyle.Italic(true)
	caretStyle := base.Foreground(lipgloss.Color(m.theme.Primary))
	// Expandable whenever there's detail to show — including the error text
	// on a failed call, so the user can click to see why it failed.
	hasOutput := msg.ToolOutput != ""
	expanded := m.expanded[msg.CallID]

	// opencode style : a diamond (status-coloured) + "agent" + the agent
	// kind in its own per-agent colour, then the task description.
	hint := ""
	if hasOutput && !expanded {
		hint = outputLineHint(msg.ToolOutput)
	}
	caret := ""
	if hasOutput {
		caret = " ▸"
		if expanded {
			caret = " ▾"
		}
	}
	// The task is the variable-width part : truncate it with "…" so the header
	// stays on one line — the full task is in the expanded body / the spawn chip.
	task := msg.ToolArg
	if task != "" {
		hintW := 0
		if hint != "" {
			hintW = lipgloss.Width("  · " + hint)
		}
		budget := (m.width - 3) - lipgloss.Width(icon+" agent "+msg.Content+"  ") - lipgloss.Width(suffix) - hintW - lipgloss.Width(caret)
		if budget < 1 {
			task = ""
		} else if lipgloss.Width(task) > budget {
			task = truncate(task, budget)
		}
	}

	nameStyle := base.Foreground(lipgloss.Color(agentColorFor(msg.Content, m.theme))).Bold(true)
	styled := base.Foreground(lipgloss.Color(col)).Render(icon) +
		muted.Render(" agent ") + nameStyle.Render(msg.Content)
	if task != "" {
		styled += muted.Render("  " + task)
	}
	styled += metaStyle.Render(suffix)
	if hint != "" {
		styled += metaStyle.Render("  · ") + linesStyle.Render(hint)
	}
	if caret != "" {
		styled += caretStyle.Render(caret)
	}
	out := styled
	if hasOutput && expanded {
		out += "\n" + m.renderAgentSummary(msg.ToolOutput, width)
	}
	return m.chipBlock(out, col) + "\n"
}

// renderTodo frames the agent's task list as an inline checklist block —
// posted once where the first task was created and updated in place as the
// plan advances. The styled lines arrive in msg.ToolOutput (see syncTodoBlock).
func (m *Messages) renderTodo(msg client.Message, width int) string {
	base := lipgloss.NewStyle().Background(lipgloss.Color(m.theme.BackgroundPanel))
	header := base.Foreground(lipgloss.Color(m.theme.Primary)).Bold(true).Render(msg.Content)
	body := header + "\n" + msg.ToolOutput
	return m.chipBlock(body, m.theme.Primary) + "\n"
}

// agentColorFor picks a stable per-agent colour from a small palette,
// hashed by the agent kind — so each sub-agent reads in its own colour
// (opencode's GetAgentColor convention).
func agentColorFor(kind string, t *theme.Theme) string {
	pal := []string{t.Secondary, t.Accent, t.Info, t.Warning, t.Success, t.Primary}
	h := 0
	for _, r := range kind {
		h = h*31 + int(r)
	}
	if h < 0 {
		h = -h
	}
	return pal[h%len(pal)]
}

// renderAgentSummary renders a sub-agent's result summary as themed,
// wrapped prose, truncated like a tool preview. Raw : the chip wraps it.
func (m *Messages) renderAgentSummary(summary string, width int) string {
	return m.capLines(strings.Trim(render.Markdown(summary, width-6, m.theme, ""), "\n"))
}

// chipBlock contains a whole tool/agent chip (header + any expanded output) in
// a tinted panel block with a subtle left bar, hugging its content — so a tool
// call reads as a contained block, not bare inline text. No fixed Width (which
// in this lipgloss is the TOTAL box width) → the panel hugs the content and
// there's no over-wide band with an unpainted right edge.
func (m *Messages) chipBlock(content, borderColor string) string {
	// Left bar tinted by the caller (the chip's status colour) so a tool call
	// reads apart from the agent reply (grey bar) and the user message
	// (Secondary bar). Full viewport width (lipgloss Width = total box width)
	// so its right edge lines up with the user band instead of stopping short.
	w := m.width
	if w < 16 {
		w = 16
	}
	return lipgloss.NewStyle().
		Border(lipgloss.ThickBorder(), false, false, false, true).
		BorderForeground(lipgloss.Color(borderColor)).
		BorderBackground(lipgloss.Color(m.theme.Background)).
		Background(lipgloss.Color(m.theme.BackgroundPanel)).
		Foreground(lipgloss.Color(m.theme.Text)).
		Padding(0, 1).
		Width(w).
		Render(content)
}

// StreamingToolLine renders a tool the model is STILL emitting as a line that
// is VISUALLY IDENTICAL to its eventual running chip — same spinner, same verb,
// same left bar (Primary) and panel background, via the same chipBlock frame —
// only without the argument/duration it doesn't have yet (shown as " …"). So
// when the real call lands the chip simply gains "<arg> · <duration> ▸" IN
// PLACE, with no jarring jump between a separate muted overlay and a chip.
// frame drives the spinner (the screen's shimmerFrame, which always advances).
// The live token count is NOT shown here — it rides the single working
// indicator above the input (which sums the whole turn).
func (m *Messages) StreamingToolLine(name string, frame int) string {
	base := lipgloss.NewStyle().Background(lipgloss.Color(m.theme.BackgroundPanel))
	meta := base.Foreground(lipgloss.Color(m.theme.TextMuted)).Faint(true)
	head := spinnerGlyph(frame)
	if v := toolVerb(stripModulePrefix(name)); v != "" {
		head += " " + v
	}
	styled := base.Foreground(lipgloss.Color(m.theme.Primary)).Render(head) + meta.Render(" …")
	return m.chipBlock(styled, m.theme.Primary)
}

// capLines truncates a rendered block to toolPreviewLines, appending a muted
// ellipsis line when it overflows.
func (m *Messages) capLines(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) <= toolPreviewLines {
		return s
	}
	lines = lines[:toolPreviewLines]
	return strings.Join(lines, "\n") + "\n" +
		lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.TextMuted)).Render("…")
}

// outputLineHint summarises hidden tool output as "N lines".
func outputLineHint(out string) string {
	n := strings.Count(strings.TrimRight(out, "\n"), "\n") + 1
	if n == 1 {
		return "1 line"
	}
	return fmt.Sprintf("%d lines", n)
}

// ToggleAt expands/collapses the tool result at the given viewport row
// (0-based, relative to the top of the messages area). Translates the row
// through the scroll offset to a committed content line, then hit-tests
// the chip map. Returns true when a chip was toggled.
func (m *Messages) ToggleAt(row int) bool {
	if row < 0 {
		return false
	}
	callID := m.chipAt[m.vp.YOffset()+row]
	if callID == "" {
		return false
	}
	m.expanded[callID] = !m.expanded[callID]
	m.rebuild()
	return true
}

// Width / Height expose the messages viewport geometry for mouse hit-tests.
func (m *Messages) Width() int  { return m.width }
func (m *Messages) Height() int { return m.vp.Height() }

// selGutter prefixes every line of a rendered block with an accent bar,
// marking it as the selected message. The block must already be rendered
// 2 cols narrower so the total width is unchanged.
func (m *Messages) selGutter(block string) string {
	mark := lipgloss.NewStyle().Foreground(lipgloss.Color(m.theme.Accent)).Render("▎")
	var b strings.Builder
	for _, ln := range strings.Split(strings.TrimRight(block, "\n"), "\n") {
		b.WriteString(mark + " " + ln + "\n")
	}
	return b.String()
}

// SelectNext / SelectPrev move the message-selection cursor. With nothing
// selected, prev starts at the most recent message and next at the first.
func (m *Messages) SelectNext() { m.moveSel(+1) }
func (m *Messages) SelectPrev() { m.moveSel(-1) }

func (m *Messages) moveSel(d int) {
	n := len(m.messages)
	if n == 0 {
		return
	}
	switch {
	case m.selIdx < 0 && d < 0:
		m.selIdx = n - 1
	case m.selIdx < 0:
		m.selIdx = 0
	default:
		m.selIdx += d
	}
	if m.selIdx < 0 {
		m.selIdx = 0
	}
	if m.selIdx >= n {
		m.selIdx = n - 1
	}
	m.rebuild()
	m.scrollToSel()
}

func (m *Messages) scrollToSel() {
	if m.selIdx < 0 || m.selIdx >= len(m.msgStart) {
		return
	}
	start := m.msgStart[m.selIdx]
	top := m.vp.YOffset()
	h := m.vp.Height()
	if start < top {
		m.vp.SetYOffset(start)
	} else if start >= top+h {
		m.vp.SetYOffset(start - h + 2)
	}
}

// SelectIndex highlights the message at i and scrolls it into view. Out-of-range
// indices clear the selection. Used by transcript search to jump to a match.
func (m *Messages) SelectIndex(i int) {
	if i < 0 || i >= len(m.messages) {
		m.ClearSelection()
		return
	}
	m.selIdx = i
	m.rebuild()
	m.scrollToSel()
}

// ClearSelection drops the selection highlight (returns to compose mode).
func (m *Messages) ClearSelection() {
	if m.selIdx >= 0 {
		m.selIdx = -1
		m.rebuild()
	}
}

// HasSelection reports whether a message is currently selected.
func (m *Messages) HasSelection() bool { return m.selIdx >= 0 }

// Empty reports whether the transcript has no messages (fresh session) — the
// chat screen shows a welcome card instead of a blank viewport.
func (m *Messages) Empty() bool { return len(m.messages) == 0 }

// AtBottom reports whether the viewport is scrolled to the latest line.
func (m *Messages) AtBottom() bool { return m.vp.AtBottom() }

// GotoBottom scrolls to the latest line and drops any selection. An explicit
// jump to the bottom (on send, replay-done, a keybinding) also RE-ATTACHES the
// sticky follow, so new content resumes pinning the view.
func (m *Messages) GotoBottom() {
	m.selIdx = -1
	m.detached = false
	m.vp.GotoBottom()
}

// BelowCount is roughly how many messages start below the current viewport —
// drives the "↓ N new" scroll-back indicator. 0 when at/near the bottom.
func (m *Messages) BelowCount() int {
	bottom := m.vp.YOffset() + m.vp.Height()
	n := 0
	for _, start := range m.msgStart {
		if start >= bottom {
			n++
		}
	}
	return n
}

// FinalizeRunning marks every still-running tool / agent chip as "ended".
// Called when a turn ends : nothing more will resolve those chips, so they
// must stop showing a perpetual "running…" spinner.
func (m *Messages) FinalizeRunning() {
	changed := false
	for i := range m.messages {
		r := m.messages[i].Role
		if isChipRole(r) && (m.messages[i].Status == "" || m.messages[i].Status == "running" || m.messages[i].Status == "pending") {
			m.messages[i].Status = "ended"
			changed = true
		}
	}
	if changed {
		m.rebuild()
	}
}

// SelectedMessage returns the currently selected message, if any.
func (m *Messages) SelectedMessage() (client.Message, bool) {
	if m.selIdx < 0 || m.selIdx >= len(m.messages) {
		return client.Message{}, false
	}
	return m.messages[m.selIdx], true
}

// SelectedContent returns the copyable text of the selected message : a
// tool's output when it has one, otherwise the message content.
func (m *Messages) SelectedContent() (string, bool) {
	if m.selIdx < 0 || m.selIdx >= len(m.messages) {
		return "", false
	}
	msg := m.messages[m.selIdx]
	if msg.Role == "tool" && msg.ToolOutput != "" {
		return msg.ToolOutput, true
	}
	return msg.Content, true
}

// ToggleAllTools expands every collapsed tool result, or collapses them all
// when none are collapsed. The keyboard path to expanding output, for use
// without a mouse (e.g. over SSH).
func (m *Messages) ToggleAllTools() {
	var ids []string
	anyCollapsed := false
	for _, msg := range m.messages {
		if (msg.Role == "tool" || msg.Role == "agent") && msg.CallID != "" && msg.ToolOutput != "" {
			ids = append(ids, msg.CallID)
			if !m.expanded[msg.CallID] {
				anyCollapsed = true
			}
		}
	}
	for _, id := range ids {
		m.expanded[id] = anyCollapsed
	}
	m.rebuild()
}

const toolPreviewLines = 10

// renderToolPreview returns a tool's plain output text, truncated to
// toolPreviewLines. Raw : the chip block frames it (left bar + panel tint).
func (m *Messages) renderToolPreview(output string) string {
	return m.capLines(strings.TrimRight(output, "\n"))
}

// renderDiffPreview renders an opencode-style diff (line-number gutter + colour-
// tinted +/- lines), truncated to toolPreviewLines. Raw : the chip block frames
// it (the ---/+++/@@ noise is already dropped by render.Diff).
func (m *Messages) renderDiffPreview(unified string, width int) string {
	return m.capLines(render.Diff(unified, width-6, m.theme))
}

// stripModulePrefix drops the "module" prefix from a tool name, leaving
// just the action. Handles both the dotted FQN ("filesystem.ls" → "ls")
// and the underscore-sanitized form OpenAI-compatible providers use
// ("filesystem__ls" → "ls", "context_builder__get_tool" → "get_tool").
func stripModulePrefix(name string) string {
	if i := strings.LastIndex(name, "__"); i >= 0 && i+2 < len(name) {
		return name[i+2:]
	}
	if i := strings.LastIndex(name, "."); i >= 0 && i+1 < len(name) {
		return name[i+1:]
	}
	return name
}

// runningSuffix is the live suffix of an in-progress chip : the elapsed
// time since it started, ticking, so the duration is visible DURING the
// run (not only at completion). Falls back to "running…" before the first
// measurable tick.
func runningSuffix(tsNano int64) string {
	if tsNano <= 0 {
		return " · running…"
	}
	ms := time.Since(time.Unix(0, tsNano)).Milliseconds()
	if d := formatDuration(ms); d != "" {
		return " · " + d
	}
	return " · running…"
}

// RefreshRunning re-renders the scrollback when a tool/agent chip is still
// running, so its live elapsed time ticks up. Called on the animation tick ;
// cheap because the per-message cache only re-renders the running chip(s)
// (their cache key is deliberately non-cacheable) and reuses every other line.
// streamRenderEveryTicks throttles the live markdown re-render of a growing
// reply. The render glamour-rebuilds the WHOLE buffer (O(n)); doing it every
// ~120ms tick is O(n²) over the reply and visibly lags long markdown answers.
// Every ~3 ticks (≈360ms) reads as "live" while cutting the glamour load ~3×.
// The committed message still renders once, in full, on commit.
const streamRenderEveryTicks = 3

// Adaptive stream-render pacing (renderStreaming). The live glamour re-render
// is reused until at least minStreamCooldown has passed, and longer when the
// render is slow : the cooldown grows to streamCostFactor × the last render's
// duration, capped at maxStreamCooldown. This bounds glamour to ~1/4 of
// wall-clock on long replies (so the loop stays responsive) while keeping short
// replies snappy — and the final crisp render always lands on commit.
const (
	minStreamCooldown = 300 * time.Millisecond
	maxStreamCooldown = 1200 * time.Millisecond
	streamCostFactor  = 4
)

func (m *Messages) RefreshRunning() {
	run := m.HasRunning()
	if run {
		m.spinFrame++
	}
	// Repaint once per tick if a chip is running (live durations/spinner) OR a
	// streamed token landed since the last frame — this is where the deferred
	// stream markdown render actually happens, coalescing a burst into one render.
	if !run && !m.streamDirty {
		return
	}
	// Throttle the (expensive, O(n)) stream-only repaint so a long reply doesn't
	// get quadratically slower : render the FIRST pending tick immediately, then
	// enter a short cooldown before the next render. When a chip is running we
	// still repaint every tick (live durations need it). streamDirty stays set
	// across the cooldown so the eventual render shows the latest text.
	if !run && m.streamDirty {
		if m.streamThrottle > 0 {
			m.streamThrottle--
			return
		}
		m.streamThrottle = streamRenderEveryTicks - 1
	}
	m.streamDirty = false
	if run {
		m.rebuild() // re-renders running chips ; its tail refresh() repaints the stream too
	} else {
		m.refresh() // just repaint committed + the freshly-rendered stream
	}
	// Pin to the latest line while attached — this is what keeps a long-running
	// tool's chip (and its live duration) IN VIEW as it works, instead of only
	// revealing it when the turn ends. Gating this on a streaming reply was the
	// bug : during tool execution there's no stream text, so it never followed.
	if !m.detached {
		m.vp.GotoBottom()
	}
}

// HasRunning reports whether any tool / agent chip is still in progress,
// so the caller keeps the animation tick alive to update live durations.
func (m *Messages) HasRunning() bool {
	for i := range m.messages {
		r := m.messages[i].Role
		if r != "tool" && r != "agent" {
			continue
		}
		switch m.messages[i].Status {
		case "completed", "done", "ok", "success", "errored", "error", "failed", "cancelled", "ended":
		default:
			return true
		}
	}
	return false
}

// humanizeTokens renders a token count compactly : raw under 1000, then "k"
// (thousands) and "M" (millions) with one decimal, a trailing ".0" dropped —
// 942 → "942", 2900 → "2.9k", 16000 → "16k", 1500000 → "1.5M". The single
// formatter for every token readout in the UI (gauges, counters, summaries).
func humanizeTokens(n int) string {
	switch {
	case n <= 0:
		return "0"
	case n < 1000:
		return strconv.Itoa(n)
	case n < 1_000_000:
		return trimUnitDecimal(float64(n)/1000) + "k"
	default:
		return trimUnitDecimal(float64(n)/1_000_000) + "M"
	}
}

// trimUnitDecimal formats a scaled value to one decimal, dropping a trailing
// ".0" so whole units read clean ("16k" not "16.0k").
func trimUnitDecimal(v float64) string {
	return strings.TrimSuffix(strconv.FormatFloat(v, 'f', 1, 64), ".0")
}

// formatDuration renders a tool's wall-clock compactly : "12ms" under a
// second, "1.2s" above. Empty for non-positive values.
func formatDuration(ms int64) string {
	switch {
	case ms <= 0:
		return ""
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	default:
		return fmt.Sprintf("%ds", ms/1000)
	}
}

func (m *Messages) renderError(msg client.Message, width int) string {
	bodyW := width - 2
	if bodyW < 20 {
		bodyW = 20
	}
	body := lipgloss.NewStyle().
		Foreground(lipgloss.Color(m.theme.Error)).
		Border(lipgloss.RoundedBorder()).
		BorderForeground(lipgloss.Color(m.theme.Error)).
		Padding(0, 1).
		Width(bodyW).
		Render("✗  " + msg.Content)
	return lipgloss.PlaceHorizontal(width, lipgloss.Center, body) + "\n"
}

func (m *Messages) Update(msg tea.Msg) tea.Cmd {
	before := m.vp.YOffset()
	var cmd tea.Cmd
	m.vp, cmd = m.vp.Update(msg)
	// Only a real scroll (the offset moved) reflects USER intent : detach when
	// they leave the bottom, re-attach when they return. Content-driven repaints
	// never reach here, so auto-follow can't be silently turned off by a race.
	if m.vp.YOffset() != before {
		m.detached = !m.vp.AtBottom()
	}
	return cmd
}

func (m *Messages) View() string {
	return m.vp.View()
}

func messagesEqual(a, b []client.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].Content != b[i].Content || a[i].Role != b[i].Role ||
			a[i].Status != b[i].Status || a[i].DurationMs != b[i].DurationMs ||
			a[i].ToolArg != b[i].ToolArg || a[i].ToolOutput != b[i].ToolOutput {
			return false
		}
	}
	return true
}
