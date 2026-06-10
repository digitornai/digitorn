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
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
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
	return &Adapter{byName: byName, order: providers, hc: &http.Client{Timeout: 30 * time.Second}, log: log}
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
			if msg.T == "MESSAGE_CREATE" {
				a.onMessage(ctx, p, msg.D, sink)
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
	}
	if json.Unmarshal(raw, &m) != nil || m.Author.Bot || m.Content == "" {
		return
	}
	var payload map[string]any
	_ = json.Unmarshal(raw, &payload)
	if err := sink(ctx, adapter.Event{
		Provider: p.Name,
		Adapter:  "discord",
		DedupKey: p.Name + ":" + m.ID,
		Source:   m.ChannelID,
		Message:  m.Content,
		Payload:  payload,
		ReplyRef: map[string]any{"channel_id": m.ChannelID, "provider": p.Name},
	}); err != nil {
		a.log.Warn("background: discord intake failed", "provider", p.Name, "err", err.Error())
	}
}

// Send posts a reply back to the originating channel via the REST API.
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
	body, _ := json.Marshal(map[string]any{"content": text})
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

// redact removes a bot token from an error string so it never reaches logs.
func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
