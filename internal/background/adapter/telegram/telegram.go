// Package telegram is the Telegram Bot API adapter: it long-polls getUpdates for
// inbound messages (offset cursor, durable) and replies via sendMessage. The bot
// token stays in the adapter — never in the durable event — and the reply
// handle carries only the chat id + provider name.
package telegram

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

const defaultAPIBase = "https://api.telegram.org"

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
	return &Adapter{byName: byName, order: providers, cursors: cursors, hc: &http.Client{Timeout: 60 * time.Second}, log: log}
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
	maxID := offset - 1
	for _, u := range updates {
		if u.UpdateID > maxID {
			maxID = u.UpdateID
		}
		if firstArm {
			continue // skip backlog on first arm
		}
		chat := strconv.FormatInt(u.Message.Chat.ID, 10)
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

// Send replies via sendMessage. The reply handle names the provider, so the
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

// redact removes a bot token from an error string so it never reaches logs.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// ── getUpdates ──────────────────────────────────────────────────────────────

type update struct {
	UpdateID int `json:"update_id"`
	Message  struct {
		MessageID int    `json:"message_id"`
		Text      string `json:"text"`
		Chat      struct {
			ID int64 `json:"id"`
		} `json:"chat"`
	} `json:"message"`
	raw map[string]any
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
