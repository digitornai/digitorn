// Package discord is the Discord bot adapter: it holds a Gateway WebSocket
// connection (identify + heartbeat + dispatch), turns inbound MESSAGE_CREATE
// events into Events, and replies via the REST API. The bot token stays in the
// adapter — never in the durable event — and the reply handle carries only the
// channel id + provider name. Messages from bots (including this one) are ignored,
// so a reply never triggers another turn.
package discord

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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/digitornai/digitorn/internal/background/adapter"
)

const (
	defaultAPIBase    = "https://discord.com/api/v10"
	defaultGatewayURL = "wss://gateway.discord.gg/?v=10&encoding=json"
	// MESSAGE_CONTENT(1<<15) | GUILD_MESSAGES(1<<9) | DIRECT_MESSAGES(1<<12).
	// MESSAGE_CONTENT is a privileged intent — it must be enabled in the bot's
	// settings (Discord Developer Portal) or message text arrives empty.
	defaultIntents = (1 << 15) | (1 << 9) | (1 << 12)

	opDispatch  = 0
	opHeartbeat = 1
	opReconnect = 7
	opInvalid   = 9
	opHello     = 10
)

// Provider is one armed bot.
type Provider struct {
	Name       string
	Token      string
	Intents    int
	APIBase    string // overridable for tests
	GatewayURL string // overridable for tests
}

// Adapter handles a set of Discord bots.
type Adapter struct {
	byName map[string]Provider
	order  []Provider
	hc     *http.Client
	log    *slog.Logger

	// prompts tracks in-flight human-in-the-loop prompts (approval / ask_user) by a
	// per-prompt nonce so an INTERACTION_CREATE (button click / modal submit) routes
	// back to the blocked Prompt call. Shared across providers (the nonce is unique).
	pmu     sync.Mutex
	prompts map[string]*pendingPrompt
}

// pendingPrompt is one awaited decision: the rendered options (button index → option),
// the free-text modal hints, and the channel the click is delivered to.
type pendingPrompt struct {
	options         []adapter.PromptOption
	textLabel       string
	textPlaceholder string
	multiline       bool
	result          chan promptHit
}

// promptHit is the user's interaction: a chosen option index, or free text (idx -1).
type promptHit struct {
	optionIdx int
	text      string
	userID    string
}

// New builds the adapter over the bots.
func New(providers []Provider, log *slog.Logger) *Adapter {
	if log == nil {
		log = slog.Default()
	}
	byName := make(map[string]Provider, len(providers))
	for i := range providers {
		if providers[i].APIBase == "" {
			providers[i].APIBase = defaultAPIBase
		}
		if providers[i].GatewayURL == "" {
			providers[i].GatewayURL = defaultGatewayURL
		}
		if providers[i].Intents == 0 {
			providers[i].Intents = defaultIntents
		}
		byName[providers[i].Name] = providers[i]
	}
	return &Adapter{
		byName:  byName,
		order:   providers,
		hc:      &http.Client{Timeout: 30 * time.Second},
		log:     log,
		prompts: map[string]*pendingPrompt{},
	}
}

func (a *Adapter) Name() string { return "discord" }

// Start runs every bot's gateway session until ctx is cancelled.
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	for _, p := range a.order {
		go a.runProvider(ctx, p, sink)
	}
	<-ctx.Done()
	return nil
}

// runProvider keeps a gateway session alive, reconnecting with backoff on any drop
// (a fresh IDENTIFY — RESUME is a follow-up) until ctx ends.
func (a *Adapter) runProvider(ctx context.Context, p Provider, sink adapter.Sink) {
	backoff := time.Second
	for ctx.Err() == nil {
		if err := a.session(ctx, p, sink); err != nil && ctx.Err() == nil {
			a.log.Warn("background: discord session ended", "provider", p.Name, "err", err.Error())
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

// session runs one gateway connection: HELLO → IDENTIFY → heartbeat + dispatch.
func (a *Adapter) session(ctx context.Context, p Provider, sink adapter.Sink) error {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, p.GatewayURL, nil)
	if err != nil {
		return fmt.Errorf("dial: %w", err)
	}
	defer conn.Close()

	var (
		mu      sync.Mutex // gorilla forbids concurrent writes
		lastSeq *int
	)
	send := func(v any) error {
		mu.Lock()
		defer mu.Unlock()
		return conn.WriteJSON(v)
	}
	seq := func() *int {
		mu.Lock()
		defer mu.Unlock()
		return lastSeq
	}

	var hello struct {
		Op int `json:"op"`
		D  struct {
			HeartbeatInterval int `json:"heartbeat_interval"`
		} `json:"d"`
	}
	if err := conn.ReadJSON(&hello); err != nil {
		return fmt.Errorf("hello: %w", err)
	}
	if hello.Op != opHello || hello.D.HeartbeatInterval <= 0 {
		return fmt.Errorf("expected HELLO, got op %d", hello.Op)
	}

	if err := send(map[string]any{
		"op": 2,
		"d": map[string]any{
			"token":      p.Token,
			"intents":    p.Intents,
			"properties": map[string]any{"os": "linux", "browser": "digitorn", "device": "digitorn"},
		},
	}); err != nil {
		return fmt.Errorf("identify: %w", err)
	}

	hbCtx, cancelHB := context.WithCancel(ctx)
	defer cancelHB()
	go func() {
		t := time.NewTicker(time.Duration(hello.D.HeartbeatInterval) * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = send(map[string]any{"op": opHeartbeat, "d": seq()})
			}
		}
	}()

	a.log.Info("background: discord connected", "provider", p.Name)

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var msg struct {
			Op int             `json:"op"`
			S  *int            `json:"s"`
			T  string          `json:"t"`
			D  json.RawMessage `json:"d"`
		}
		if err := conn.ReadJSON(&msg); err != nil {
			return err
		}
		if msg.S != nil {
			mu.Lock()
			lastSeq = msg.S
			mu.Unlock()
		}
		switch msg.Op {
		case opHeartbeat: // server asked for an immediate heartbeat
			_ = send(map[string]any{"op": opHeartbeat, "d": seq()})
		case opReconnect, opInvalid:
			return fmt.Errorf("gateway requested reconnect (op %d)", msg.Op)
		case opDispatch:
			switch msg.T {
			case "MESSAGE_CREATE":
				a.onMessage(ctx, p, msg.D, sink)
			case "INTERACTION_CREATE":
				a.onInteraction(ctx, p, msg.D)
			}
		}
	}
}

// onMessage turns a MESSAGE_CREATE into an Event — skipping bot authors (incl.
// this bot) so a reply never feeds back as a new turn.
func (a *Adapter) onMessage(ctx context.Context, p Provider, raw json.RawMessage, sink adapter.Sink) {
	var m struct {
		ID        string `json:"id"`
		ChannelID string `json:"channel_id"`
		Content   string `json:"content"`
		Author    struct {
			ID  string `json:"id"`
			Bot bool   `json:"bot"`
		} `json:"author"`
		Attachments []struct {
			Filename    string `json:"filename"`
			ContentType string `json:"content_type"`
			Size        int64  `json:"size"`
			URL         string `json:"url"`
		} `json:"attachments"`
	}
	if json.Unmarshal(raw, &m) != nil || m.Author.Bot {
		return
	}
	atts := make([]adapter.Attachment, 0, len(m.Attachments))
	for _, at := range m.Attachments {
		if at.URL == "" {
			continue
		}
		atts = append(atts, adapter.Attachment{
			Filename:    at.Filename,
			ContentType: at.ContentType,
			Size:        at.Size,
			Ref:         map[string]any{"url": at.URL},
		})
	}
	if m.Content == "" && len(atts) == 0 {
		return // an empty, attachment-less message (e.g. a join notice) — nothing to do
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if err := sink(ctx, adapter.Event{
		Provider:    p.Name,
		Adapter:     "discord",
		DedupKey:    p.Name + ":" + m.ID,
		Source:      m.ChannelID,
		Message:     m.Content,
		Payload:     payload,
		Attachments: atts,
		ReplyRef:    map[string]any{"channel_id": m.ChannelID, "provider": p.Name},
	}); err != nil {
		a.log.Warn("background: discord intake failed", "provider", p.Name, "err", err.Error())
	}
}

// FetchMedia downloads an attachment from its Discord CDN URL (public, no auth) so
// the processor can upload the bytes to the daemon. Satisfies adapter.MediaFetcher.
func (a *Adapter) FetchMedia(ctx context.Context, att adapter.Attachment) ([]byte, string, error) {
	rawURL, _ := att.Ref["url"].(string)
	if rawURL == "" {
		return nil, "", fmt.Errorf("discord: attachment has no url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("discord: fetch media %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20)) // 32 MB cap
	if err != nil {
		return nil, "", err
	}
	mime := att.ContentType
	if mime == "" {
		mime = resp.Header.Get("Content-Type")
	}
	return data, mime, nil
}

// maxMessageChars is Discord's hard per-message limit (2000 for regular accounts).
// A reply longer than this is split into several messages — otherwise sendMessage
// returns 400 (BASE_TYPE_MAX_LENGTH) and the answer never reaches the channel.
const maxMessageChars = 2000

// Send posts a reply back to the originating channel via the REST API, chunked to
// Discord's per-message limit so long agent answers are delivered in full.
func (a *Adapter) Send(ctx context.Context, replyRef map[string]any, text string) error {
	name, _ := replyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return fmt.Errorf("discord: no provider %q for reply", name)
	}
	channel, _ := replyRef["channel_id"].(string)
	if channel == "" {
		return fmt.Errorf("discord: no channel_id in reply handle")
	}
	for _, chunk := range chunkText(text, maxMessageChars) {
		if err := a.postOne(ctx, p, channel, chunk); err != nil {
			return err
		}
	}
	return nil
}

// Typing posts a transient typing indicator to the channel (Discord shows it for
// ~10 s). Satisfies adapter.Typer so the processor can keep presence alive during a
// turn. Best-effort: a failure here never blocks the reply.
func (a *Adapter) Typing(ctx context.Context, replyRef map[string]any) error {
	name, _ := replyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return fmt.Errorf("discord: no provider %q", name)
	}
	channel, _ := replyRef["channel_id"].(string)
	if channel == "" {
		return fmt.Errorf("discord: no channel_id")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.APIBase+"/channels/"+channel+"/typing", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+p.Token)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("discord: typing: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
	return nil
}

// postOne sends a single (already-bounded) message to a channel.
func (a *Adapter) postOne(ctx context.Context, p Provider, channel, content string) error {
	body, _ := json.Marshal(map[string]any{"content": content})
	endpoint := p.APIBase + "/channels/" + channel + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bot "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("discord: sendMessage: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("discord: sendMessage %d: %s", resp.StatusCode, raw)
	}
	return nil
}

// chunkText splits s into pieces of at most max runes (Discord counts codepoints),
// breaking at a newline/space near the boundary so words/lines aren't cut mid-token.
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

// redact removes a bot token from an error string so it never reaches logs.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// ── Human-in-the-loop prompts (tool approval / ask_user) ──────────────────────

// Discord message-component / interaction-response type constants.
const (
	compActionRow = 1
	compButton    = 2
	compTextInput = 4

	respUpdateMessage = 7 // edit the component's source message
	respModal         = 9 // open a modal (text input)

	btnPrimary   = 1
	btnSecondary = 2
	btnDanger    = 4

	itComponent = 3 // MESSAGE_COMPONENT (button click)
	itModal     = 5 // MODAL_SUBMIT
)

// pendingPrompt fields the modal needs are stored so a click that opens it can carry
// the agent's label/placeholder/multiline hint.
//
// Prompt renders a decision in the channel — a button per option, plus a "type your
// answer" button that opens a modal when free text is allowed — and blocks until the
// user answers or ctx ends. Satisfies adapter.Prompter, so the processor drives
// tool-approval and ask_user questions on Discord with zero Discord-specific code.
func (a *Adapter) Prompt(ctx context.Context, req adapter.PromptRequest) (adapter.PromptResponse, error) {
	name, _ := req.ReplyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return adapter.PromptResponse{}, fmt.Errorf("discord: no provider %q for prompt", name)
	}
	channel, _ := req.ReplyRef["channel_id"].(string)
	if channel == "" {
		return adapter.PromptResponse{}, fmt.Errorf("discord: no channel_id for prompt")
	}
	nonce, err := newNonce()
	if err != nil {
		return adapter.PromptResponse{}, err
	}

	pp := &pendingPrompt{
		options:         req.Options,
		textLabel:       req.TextLabel,
		textPlaceholder: req.TextPlaceholder,
		multiline:       req.Multiline,
		result:          make(chan promptHit, 1),
	}
	a.pmu.Lock()
	a.prompts[nonce] = pp
	a.pmu.Unlock()
	defer func() {
		a.pmu.Lock()
		delete(a.prompts, nonce)
		a.pmu.Unlock()
	}()

	msgID, err := a.postPrompt(ctx, p, channel, req, nonce)
	if err != nil {
		return adapter.PromptResponse{}, err
	}

	select {
	case hit := <-pp.result:
		if hit.optionIdx >= 0 && hit.optionIdx < len(req.Options) {
			return adapter.PromptResponse{OptionID: req.Options[hit.optionIdx].ID, UserID: hit.userID}, nil
		}
		return adapter.PromptResponse{Text: hit.text, UserID: hit.userID}, nil
	case <-ctx.Done():
		a.expirePrompt(p, channel, msgID) // best-effort: drop the buttons
		return adapter.PromptResponse{}, ctx.Err()
	}
}

// postPrompt posts the prompt message with its component rows; returns the message id.
func (a *Adapter) postPrompt(ctx context.Context, p Provider, channel string, req adapter.PromptRequest, nonce string) (string, error) {
	content := req.Title
	if req.Body != "" {
		if content != "" {
			content += "\n"
		}
		content += req.Body
	}
	body, _ := json.Marshal(map[string]any{
		"content":    clip(content, maxMessageChars),
		"components": promptComponents(req, nonce),
	})
	endpoint := p.APIBase + "/channels/" + channel + "/messages"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Authorization", "Bot "+p.Token)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("discord: postPrompt: %s", redact(err.Error(), p.Token))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("discord: postPrompt %d: %s", resp.StatusCode, raw)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(raw, &out)
	return out.ID, nil
}

// promptComponents builds Discord action rows: a button per option (≤5 per row, ≤5
// rows), plus a trailing "type your answer" button when free text is allowed.
func promptComponents(req adapter.PromptRequest, nonce string) []any {
	var buttons []any
	for i, opt := range req.Options {
		buttons = append(buttons, map[string]any{
			"type":      compButton,
			"style":     styleToDiscord(opt.Style),
			"label":     clip(opt.Label, 80),
			"custom_id": "a:" + nonce + ":" + strconv.Itoa(i),
		})
	}
	if req.AllowText {
		label := req.TextLabel
		if label == "" {
			label = "Répondre"
		}
		buttons = append(buttons, map[string]any{
			"type":      compButton,
			"style":     btnSecondary,
			"label":     clip("✏️ "+label, 80),
			"custom_id": "a:" + nonce + ":t",
		})
	}
	if len(buttons) == 0 {
		return nil
	}
	var rows []any
	for i := 0; i < len(buttons); i += 5 {
		end := min(i+5, len(buttons))
		rows = append(rows, map[string]any{"type": compActionRow, "components": buttons[i:end]})
		if len(rows) == 5 {
			break
		}
	}
	return rows
}

// onInteraction routes a button click / modal submit back to the blocked Prompt and
// acknowledges the interaction (edit the message, or open a modal) within Discord's
// 3-second deadline.
func (a *Adapter) onInteraction(ctx context.Context, p Provider, raw json.RawMessage) {
	var it struct {
		ID    string `json:"id"`
		Token string `json:"token"`
		Type  int    `json:"type"`
		Data  struct {
			CustomID   string `json:"custom_id"`
			Components []struct {
				Components []struct {
					CustomID string `json:"custom_id"`
					Value    string `json:"value"`
				} `json:"components"`
			} `json:"components"`
		} `json:"data"`
		Member struct {
			User struct {
				ID string `json:"id"`
			} `json:"user"`
		} `json:"member"`
		User struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if json.Unmarshal(raw, &it) != nil {
		return
	}
	userID := it.Member.User.ID
	if userID == "" {
		userID = it.User.ID
	}

	switch it.Type {
	case itComponent:
		nonce, rest, ok := parseCustomID(it.Data.CustomID, "a")
		if !ok {
			return
		}
		if rest == "t" { // free-text answer → open a modal
			a.respondModal(ctx, p, it.ID, it.Token, nonce)
			return
		}
		idx, err := strconv.Atoi(rest)
		if err != nil {
			return
		}
		pp := a.takeHit(nonce, promptHit{optionIdx: idx, userID: userID})
		verdict := "Enregistré"
		if pp != nil && idx >= 0 && idx < len(pp.options) {
			verdict = pp.options[idx].Label
		}
		a.ackUpdate(ctx, p, it.ID, it.Token, "**"+verdict+"** — <@"+userID+">", pp == nil)
	case itModal:
		nonce, _, ok := parseCustomID(it.Data.CustomID, "m")
		if !ok {
			return
		}
		answer := ""
		for _, row := range it.Data.Components {
			for _, c := range row.Components {
				if c.CustomID == "answer" {
					answer = c.Value
				}
			}
		}
		pp := a.takeHit(nonce, promptHit{optionIdx: -1, text: answer, userID: userID})
		a.ackUpdate(ctx, p, it.ID, it.Token, "✅ Réponse enregistrée — <@"+userID+">", pp == nil)
	}
}

// takeHit delivers the interaction to the waiting Prompt (non-blocking; first wins)
// and returns the pending prompt (nil if it already resolved / expired).
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

// ackUpdate edits the component's source message (remove buttons + show the verdict).
// When the waiter is gone (expired/superseded) it still acks so Discord doesn't show
// "interaction failed", just with an expiry note.
func (a *Adapter) ackUpdate(ctx context.Context, p Provider, id, token, content string, expired bool) {
	if expired {
		content = "⏰ Cette demande a expiré."
	}
	a.interactionCallback(ctx, p, id, token, map[string]any{
		"type": respUpdateMessage,
		"data": map[string]any{"content": clip(content, maxMessageChars), "components": []any{}},
	})
}

// respondModal answers a button click by opening a single-field text-input modal,
// honouring the agent's label/placeholder/multiline hint when still known.
func (a *Adapter) respondModal(ctx context.Context, p Provider, id, token, nonce string) {
	label, placeholder, style := "Réponse", "", 1
	a.pmu.Lock()
	pp := a.prompts[nonce]
	a.pmu.Unlock()
	if pp != nil {
		if pp.textLabel != "" {
			label = pp.textLabel
		}
		placeholder = pp.textPlaceholder
		if pp.multiline {
			style = 2 // paragraph
		}
	}
	input := map[string]any{
		"type":      compTextInput,
		"custom_id": "answer",
		"style":     style,
		"label":     clip(label, 45),
		"required":  true,
	}
	if placeholder != "" {
		input["placeholder"] = clip(placeholder, 100)
	}
	a.interactionCallback(ctx, p, id, token, map[string]any{
		"type": respModal,
		"data": map[string]any{
			"custom_id":  "m:" + nonce,
			"title":      "Votre réponse",
			"components": []any{map[string]any{"type": compActionRow, "components": []any{input}}},
		},
	})
}

// interactionCallback POSTs an interaction response (authorized by the interaction
// token in the URL — no bot auth needed). Best-effort: a failure is logged, not fatal.
func (a *Adapter) interactionCallback(ctx context.Context, p Provider, id, token string, payload map[string]any) {
	body, _ := json.Marshal(payload)
	endpoint := p.APIBase + "/interactions/" + id + "/" + token + "/callback"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		a.log.Warn("background: discord interaction ack failed", "err", redact(err.Error(), p.Token))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
}

// expirePrompt drops the buttons from a prompt message whose Prompt call ended (ctx
// cancelled) before any click. Best-effort.
func (a *Adapter) expirePrompt(p Provider, channel, msgID string) {
	if msgID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{"content": "⏰ Demande expirée.", "components": []any{}})
	endpoint := p.APIBase + "/channels/" + channel + "/messages/" + msgID
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, endpoint, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bot "+p.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.hc.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<14))
}

// parseCustomID splits "<prefix>:<nonce>[:<rest>]"; ok=false if the prefix mismatches.
// rest is optional — a modal id is "m:<nonce>" (no third segment).
func parseCustomID(s, prefix string) (nonce, rest string, ok bool) {
	parts := strings.SplitN(s, ":", 3)
	if len(parts) < 2 || parts[0] != prefix {
		return "", "", false
	}
	if len(parts) == 3 {
		rest = parts[2]
	}
	return parts[1], rest, true
}

func styleToDiscord(style string) int {
	switch style {
	case "danger":
		return btnDanger
	case "secondary":
		return btnSecondary
	default:
		return btnPrimary
	}
}

func newNonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// clip truncates s to max runes (Discord counts codepoints), adding an ellipsis.
func clip(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}
