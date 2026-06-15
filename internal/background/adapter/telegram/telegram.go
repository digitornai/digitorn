// Package telegram is the Telegram Bot API adapter: it long-polls getUpdates for
// inbound messages (offset cursor, durable) and replies via sendMessage. The bot
// token stays in the adapter — never in the durable event — and the reply
// handle carries only the chat id + provider name.
package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

const defaultAPIBase = "https://api.telegram.org"

// firstArmFreshWindow bounds what counts as "live" on a fresh cursor: on the very
// first poll of a never-seen bot we skip genuine backlog (old messages we must not
// replay) but STILL deliver a message the user just sent — otherwise a brand-new bot
// silently drops the first message. Telegram stamps each message with a Unix date, so
// "recent" = within this window of now.
const firstArmFreshWindow = 90 * time.Second

// Provider is one armed bot.
type Provider struct {
	Name      string
	Token     string
	CursorKey string
	Interval  time.Duration // pause between long-polls (default 1s)
	APIBase   string        // overridable for tests
}

// CursorStore persists the getUpdates offset per provider.
type CursorStore interface {
	Cursor(ctx context.Context, key string) string
	SetCursor(ctx context.Context, key, value string) error
}

// Adapter handles a set of bots.
type Adapter struct {
	byName  map[string]Provider
	order   []Provider
	cursors CursorStore
	hc      *http.Client
	log     *slog.Logger

	// prompts tracks in-flight human-in-the-loop prompts (approval / ask_user) by a
	// per-prompt nonce so a callback_query (inline-button click) routes back to the
	// blocked Prompt call. Shared across providers (the nonce is unique).
	pmu     sync.Mutex
	prompts map[string]*pendingPrompt
}

// pendingPrompt is one awaited decision: the rendered options (button index → option)
// + the channel that delivers the click.
type pendingPrompt struct {
	options []adapter.PromptOption
	result  chan promptHit
}

type promptHit struct {
	optionIdx int
	userID    string
}

// New builds the adapter over the bots.
func New(providers []Provider, cursors CursorStore, log *slog.Logger) *Adapter {
	if log == nil {
		log = slog.Default()
	}
	byName := make(map[string]Provider, len(providers))
	for i := range providers {
		if providers[i].APIBase == "" {
			providers[i].APIBase = defaultAPIBase
		}
		if providers[i].Interval <= 0 {
			providers[i].Interval = time.Second
		}
		byName[providers[i].Name] = providers[i]
	}
	return &Adapter{
		byName:  byName,
		order:   providers,
		cursors: cursors,
		hc:      &http.Client{Timeout: 60 * time.Second},
		log:     log,
		prompts: map[string]*pendingPrompt{},
	}
}

func (a *Adapter) Name() string { return "telegram" }

// Start long-polls every bot until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	for _, p := range a.order {
		go a.loop(ctx, p, sink)
	}
	<-ctx.Done()
	return nil
}

func (a *Adapter) loop(ctx context.Context, p Provider, sink adapter.Sink) {
	for {
		if ctx.Err() != nil {
			return
		}
		a.pollOnce(ctx, p, sink)
		select {
		case <-ctx.Done():
			return
		case <-time.After(p.Interval):
		}
	}
}

// pollOnce fetches the next batch from the stored offset and emits each update.
// First arm (no offset): ack the backlog without replaying it.
func (a *Adapter) pollOnce(ctx context.Context, p Provider, sink adapter.Sink) {
	offset := 0
	cur := a.cursors.Cursor(ctx, p.CursorKey)
	firstArm := cur == ""
	if !firstArm {
		offset, _ = strconv.Atoi(cur)
	}
	updates, err := a.getUpdates(ctx, p, offset)
	if err != nil {
		a.log.Warn("background: telegram getUpdates failed", "provider", p.Name, "err", err.Error())
		return
	}
	if len(updates) > 0 {
		a.log.Info("background: telegram poll", "provider", p.Name, "updates", len(updates), "first_arm", firstArm, "offset", offset)
	}
	maxID := offset - 1
	for _, u := range updates {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
		// Inline-button click (approval / ask_user) → route to the blocked Prompt;
		// it is never a chat message and is processed even on a fresh cursor.
		if u.CallbackQuery != nil {
			a.onCallback(ctx, p, u.CallbackQuery)
			continue
		}
		// Only real text messages become Events (skip edits / joins / other types).
		if u.Message.Chat.ID == 0 {
			continue
		}
		// On a fresh cursor, skip genuine backlog (old messages we must not replay on
		// a (re)start) but STILL deliver a message the user just sent — otherwise a
		// brand-new bot silently drops the very first message. Recency = the message's
		// own Telegram date.
		if firstArm && !recent(u.Message.Date) {
			a.log.Info("background: telegram skip backlog", "provider", p.Name, "update", u.UpdateID, "date", u.Message.Date)
			continue
		}
		chat := strconv.FormatInt(u.Message.Chat.ID, 10)
		a.log.Info("background: telegram intake", "provider", p.Name, "update", u.UpdateID, "chat", chat, "text", truncate(u.Message.Text, 40))
		if err := sink(ctx, adapter.Event{
			Provider: p.Name,
			Adapter:  "telegram",
			DedupKey: p.Name + ":" + strconv.Itoa(u.UpdateID),
			Source:   chat,
			Message:  u.Message.Text,
			Payload:  u.raw,
			ReplyRef: map[string]any{"chat_id": chat, "provider": p.Name},
		}); err != nil {
			a.log.Warn("background: telegram intake failed", "provider", p.Name, "err", err.Error())
		}
	}
	if maxID >= offset {
		_ = a.cursors.SetCursor(ctx, p.CursorKey, strconv.Itoa(maxID+1))
	}
}

// maxMessageChars is Telegram's hard per-message limit (4096 UTF-16 code units; we
// bound by runes, which is conservative). A reply longer than this is split into
// several messages — otherwise sendMessage returns 400 (MESSAGE_TOO_LONG) and the
// answer never reaches the chat.
const maxMessageChars = 4096

// Send replies via sendMessage, chunked to Telegram's per-message limit so long
// agent answers are delivered in full. The reply handle names the provider, so the
// matching bot token is used; the token never leaves the adapter.
func (a *Adapter) Send(ctx context.Context, replyRef map[string]any, text string) error {
	name, _ := replyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return fmt.Errorf("telegram: no provider %q for reply", name)
	}
	chat, _ := replyRef["chat_id"].(string)
	if chat == "" {
		return fmt.Errorf("telegram: no chat_id in reply handle")
	}
	for _, chunk := range chunkText(text, maxMessageChars) {
		if err := a.sendOne(ctx, p, chat, chunk); err != nil {
			return err
		}
	}
	return nil
}

// sendOne posts a single (already-bounded) message to a chat.
func (a *Adapter) sendOne(ctx context.Context, p Provider, chat, text string) error {
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "text": text})
	endpoint := p.APIBase + "/bot" + url.PathEscape(p.Token) + "/sendMessage"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendMessage: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("telegram: sendMessage %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// chunkText splits s into pieces of at most max runes (Telegram counts UTF-16 units;
// runes are conservative and never split a multibyte codepoint), breaking at a
// newline/space near the boundary so words/lines aren't cut mid-token.
func chunkText(s string, max int) []string {
	if max <= 0 {
		max = maxMessageChars
	}
	r := []rune(strings.TrimSpace(s))
	if len(r) == 0 {
		return nil
	}
	var out []string
	for len(r) > 0 {
		if len(r) <= max {
			out = append(out, string(r))
			break
		}
		cut := max
		for i := max - 1; i > max/2; i-- {
			if r[i] == '\n' || r[i] == ' ' {
				cut = i
				break
			}
		}
		out = append(out, strings.TrimRight(string(r[:cut]), " \n"))
		for cut < len(r) && (r[cut] == '\n' || r[cut] == ' ') {
			cut++
		}
		r = r[cut:]
	}
	return out
}

// Typing sends a "typing…" chat action (Telegram shows it for ~5 s). Satisfies
// adapter.Typer so the processor keeps presence alive during a turn. Best-effort.
func (a *Adapter) Typing(ctx context.Context, replyRef map[string]any) error {
	name, _ := replyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return fmt.Errorf("telegram: no provider %q for typing", name)
	}
	chat, _ := replyRef["chat_id"].(string)
	if chat == "" {
		return fmt.Errorf("telegram: no chat_id in reply handle")
	}
	body, _ := json.Marshal(map[string]any{"chat_id": chat, "action": "typing"})
	endpoint := p.APIBase + "/bot" + url.PathEscape(p.Token) + "/sendChatAction"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("telegram: sendChatAction: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	return nil
}

// truncate shortens s for log lines.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// redact removes a bot token from an error string so it never reaches logs.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// ── Human-in-the-loop prompts (tool approval / ask_user) ──────────────────────

// Prompt renders a decision as an inline keyboard and blocks until the user taps a
// button (or ctx ends). Satisfies adapter.Prompter, so the processor drives
// tool-approval and ask_user choices on Telegram with no Telegram-specific code.
// Free-text prompts (no options) are unsupported here — the processor degrades them.
func (a *Adapter) Prompt(ctx context.Context, req adapter.PromptRequest) (adapter.PromptResponse, error) {
	name, _ := req.ReplyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return adapter.PromptResponse{}, fmt.Errorf("telegram: no provider %q for prompt", name)
	}
	chat, _ := req.ReplyRef["chat_id"].(string)
	if chat == "" {
		return adapter.PromptResponse{}, fmt.Errorf("telegram: no chat_id for prompt")
	}
	if len(req.Options) == 0 {
		return adapter.PromptResponse{}, fmt.Errorf("telegram: free-text prompt unsupported")
	}
	nonce, err := newNonce()
	if err != nil {
		return adapter.PromptResponse{}, err
	}
	pp := &pendingPrompt{options: req.Options, result: make(chan promptHit, 1)}
	a.pmu.Lock()
	a.prompts[nonce] = pp
	a.pmu.Unlock()
	defer func() {
		a.pmu.Lock()
		delete(a.prompts, nonce)
		a.pmu.Unlock()
	}()

	msgID, err := a.postPrompt(ctx, p, chat, promptText(req), nonce, req.Options)
	if err != nil {
		return adapter.PromptResponse{}, err
	}

	select {
	case hit := <-pp.result:
		if hit.optionIdx >= 0 && hit.optionIdx < len(req.Options) {
			return adapter.PromptResponse{OptionID: req.Options[hit.optionIdx].ID, UserID: hit.userID}, nil
		}
		return adapter.PromptResponse{UserID: hit.userID}, nil
	case <-ctx.Done():
		a.expirePrompt(p, chat, msgID)
		return adapter.PromptResponse{}, ctx.Err()
	}
}

// postPrompt sends the prompt message with its inline keyboard; returns the message id.
func (a *Adapter) postPrompt(ctx context.Context, p Provider, chat, text, nonce string, opts []adapter.PromptOption) (int, error) {
	out, err := a.callJSON(ctx, p, "sendMessage", map[string]any{
		"chat_id":      chat,
		"text":         text,
		"reply_markup": map[string]any{"inline_keyboard": keyboard(nonce, opts)},
	})
	if err != nil {
		return 0, err
	}
	var r struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	_ = json.Unmarshal(out, &r)
	return r.Result.MessageID, nil
}

// keyboard lays the options out as inline-keyboard rows (≤3 buttons/row). custom_data
// encodes the prompt nonce + option index so a click routes back to the right Prompt.
func keyboard(nonce string, opts []adapter.PromptOption) [][]map[string]any {
	var rows [][]map[string]any
	var row []map[string]any
	for i, o := range opts {
		row = append(row, map[string]any{
			"text":          truncate(o.Label, 60),
			"callback_data": "a:" + nonce + ":" + strconv.Itoa(i),
		})
		if len(row) == 3 {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	return rows
}

// onCallback routes an inline-button click to the blocked Prompt, ACKs it (stops the
// client spinner), and edits the message to the chosen verdict (buttons removed).
func (a *Adapter) onCallback(ctx context.Context, p Provider, cb *callbackQuery) {
	_, _ = a.callJSON(ctx, p, "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID})
	nonce, rest, ok := parseCB(cb.Data)
	if !ok {
		return
	}
	idx, err := strconv.Atoi(rest)
	if err != nil {
		return
	}
	userID := strconv.FormatInt(cb.From.ID, 10)
	pp := a.takeHit(nonce, promptHit{optionIdx: idx, userID: userID})
	verdict := "⏰ Expiré"
	if pp != nil && idx >= 0 && idx < len(pp.options) {
		verdict = pp.options[idx].Label + " — " + userID
	}
	a.editPrompt(ctx, p, cb.Message.Chat.ID, cb.Message.MessageID, verdict)
}

// takeHit delivers the click to the waiting Prompt (non-blocking; first wins) and
// returns the pending prompt (nil if already resolved / expired).
func (a *Adapter) takeHit(nonce string, hit promptHit) *pendingPrompt {
	a.pmu.Lock()
	pp := a.prompts[nonce]
	a.pmu.Unlock()
	if pp == nil {
		return nil
	}
	select {
	case pp.result <- hit:
	default:
	}
	return pp
}

// editPrompt rewrites the prompt message to its verdict and drops the keyboard.
func (a *Adapter) editPrompt(ctx context.Context, p Provider, chatID int64, msgID int, text string) {
	a.callJSON(ctx, p, "editMessageText", map[string]any{ //nolint:errcheck
		"chat_id":      chatID,
		"message_id":   msgID,
		"text":         text,
		"reply_markup": map[string]any{"inline_keyboard": [][]any{}},
	})
}

// expirePrompt drops the keyboard from a prompt whose Prompt call ended (ctx) with no
// click. Best-effort.
func (a *Adapter) expirePrompt(p Provider, chat string, msgID int) {
	if msgID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	chatID, _ := strconv.ParseInt(chat, 10, 64)
	a.editPrompt(ctx, p, chatID, msgID, "⏰ Demande expirée.")
}

// callJSON POSTs a Bot API method and returns the raw body. Best-effort errors are
// the caller's to log; the token never reaches an error string.
func (a *Adapter) callJSON(ctx context.Context, p Provider, method string, body map[string]any) ([]byte, error) {
	raw, _ := json.Marshal(body)
	endpoint := p.APIBase + "/bot" + url.PathEscape(p.Token) + "/" + method
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("telegram: %s: %s", method, redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("telegram: %s %d: %s", method, resp.StatusCode, out)
	}
	return out, nil
}

func promptText(req adapter.PromptRequest) string {
	t := req.Title
	if req.Body != "" {
		if t != "" {
			t += "\n"
		}
		t += req.Body
	}
	if t == "" {
		t = "Décision requise."
	}
	return truncate(t, 4000)
}

func parseCB(s string) (nonce, rest string, ok bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 || parts[0] != "a" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// compile-time guard: the adapter satisfies the optional Prompter capability.
var _ adapter.Prompter = (*Adapter)(nil)

// ── getUpdates ──────────────────────────────────────────────────────────────

type update struct {
	UpdateID int `json:"update_id"`
	Message  struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		Date      int64  `json:"date"` // Unix seconds; used to tell a fresh message from backlog
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	// CallbackQuery is an inline-button click (approval / ask_user). Routed to the
	// blocked Prompt, never delivered as a chat message.
	CallbackQuery *callbackQuery `json:"callback_query"`
	raw           map[string]any
}

// callbackQuery is the subset of Telegram's callback_query we consume.
type callbackQuery struct {
	ID   string `json:"id"`
	Data string `json:"data"`
	From struct {
		ID int64 `json:"id"`
	} `json:"from"`
	Message struct {
		MessageID int `json:"message_id"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
}

// recent reports whether a message's Telegram date is within the fresh window of now
// (a message the user just sent), vs genuine backlog to skip on a fresh cursor.
func recent(unixSecs int64) bool {
	return unixSecs > 0 && time.Since(time.Unix(unixSecs, 0)) <= firstArmFreshWindow
}

func (a *Adapter) getUpdates(ctx context.Context, p Provider, offset int) ([]update, error) {
	q := url.Values{}
	if offset > 0 {
		q.Set("offset", strconv.Itoa(offset))
	}
	q.Set("timeout", "0") // non-blocking; the loop paces itself via p.Interval
	q.Set("limit", "100")
	endpoint := p.APIBase + "/bot" + url.PathEscape(p.Token) + "/getUpdates?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("getUpdates: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("telegram getUpdates %d: %s", resp.StatusCode, raw)
	}
	var env struct {
		OK     bool              `json:"ok"`
		Result []json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, err
	}
	if !env.OK {
		return nil, fmt.Errorf("telegram getUpdates: ok=false")
	}
	out := make([]update, 0, len(env.Result))
	for _, r := range env.Result {
		var u update
		if json.Unmarshal(r, &u) != nil {
			continue
		}
		_ = json.Unmarshal(r, &u.raw)
		out = append(out, u)
	}
	return out, nil
}
