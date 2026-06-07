// Package whatsapp is the WhatsApp Cloud API adapter: inbound messages arrive as
// Meta webhook POSTs (HMAC-verified) and replies go out via the Graph API. The
// access token stays in the adapter; the reply handle carries only the
// recipient + provider name.
package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/background/adapter"
)

const (
	defaultAPIBase    = "https://graph.facebook.com"
	defaultAPIVersion = "v19.0"
	maxBodyBytes      = 1 << 20
)

// Provider is one WhatsApp number/app binding.
type Provider struct {
	Name          string
	Path          string // inbound webhook path
	AppSecret     string // HMAC (X-Hub-Signature-256) verification
	VerifyToken   string // GET subscription challenge
	AccessToken   string // Graph API bearer
	PhoneNumberID string // default sender id
	APIBase       string
	APIVersion    string
}

// Adapter handles a set of WhatsApp providers.
type Adapter struct {
	byPath map[string]Provider
	byName map[string]Provider
	hc     *http.Client
	log    *slog.Logger

	mu   sync.RWMutex
	sink adapter.Sink

	AllowInsecure bool // tests: skip nothing today, reserved
}

// New builds the adapter over the providers.
func New(providers []Provider, log *slog.Logger) *Adapter {
	if log == nil {
		log = slog.Default()
	}
	byPath := make(map[string]Provider, len(providers))
	byName := make(map[string]Provider, len(providers))
	for _, p := range providers {
		if p.APIBase == "" {
			p.APIBase = defaultAPIBase
		}
		if p.APIVersion == "" {
			p.APIVersion = defaultAPIVersion
		}
		byPath[p.Path] = p
		byName[p.Name] = p
	}
	return &Adapter{byPath: byPath, byName: byName, hc: &http.Client{Timeout: 20 * time.Second}, log: log}
}

func (a *Adapter) Name() string { return "whatsapp" }

// Start stores the sink; serving is done by the service mounting Handler().
func (a *Adapter) Start(ctx context.Context, sink adapter.Sink) error {
	a.mu.Lock()
	a.sink = sink
	a.mu.Unlock()
	<-ctx.Done()
	return nil
}

func (a *Adapter) currentSink() adapter.Sink {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.sink
}

// Handler is the inbound HTTP handler the service mounts.
func (a *Adapter) Handler() http.Handler { return http.HandlerFunc(a.serve) }

func (a *Adapter) serve(w http.ResponseWriter, r *http.Request) {
	p, ok := a.byPath[r.URL.Path]
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	// Subscription verification handshake (Meta calls GET once at setup).
	if r.Method == http.MethodGet {
		q := r.URL.Query()
		if q.Get("hub.mode") == "subscribe" && q.Get("hub.verify_token") == p.VerifyToken && p.VerifyToken != "" {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(q.Get("hub.challenge")))
			return
		}
		http.Error(w, "verification failed", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	sink := a.currentSink()
	if sink == nil {
		http.Error(w, "not ready", http.StatusServiceUnavailable)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || int64(len(body)) > maxBodyBytes {
		http.Error(w, "bad body", http.StatusRequestEntityTooLarge)
		return
	}
	if p.AppSecret != "" && !verifyHMAC(p.AppSecret, body, r.Header.Get("X-Hub-Signature-256")) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	for _, ev := range parseInbound(body, p) {
		if err := sink(r.Context(), ev); err != nil {
			a.log.Warn("background: whatsapp intake failed", "provider", p.Name, "err", err.Error())
		}
	}
	// Always 200 so Meta doesn't retry (we've durably taken in what we parsed).
	w.WriteHeader(http.StatusOK)
}

// Send replies via the Graph API. The reply handle names the provider, so the
// matching access token is used; the token never leaves the adapter.
func (a *Adapter) Send(ctx context.Context, replyRef map[string]any, text string) error {
	name, _ := replyRef["provider"].(string)
	p, ok := a.byName[name]
	if !ok {
		return fmt.Errorf("whatsapp: no provider %q for reply", name)
	}
	to, _ := replyRef["to"].(string)
	if to == "" {
		return fmt.Errorf("whatsapp: no recipient in reply handle")
	}
	pnid, _ := replyRef["phone_number_id"].(string)
	if pnid == "" {
		pnid = p.PhoneNumberID
	}
	payload, _ := json.Marshal(map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]any{"body": text},
	})
	endpoint := p.APIBase + "/" + p.APIVersion + "/" + pnid + "/messages"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.AccessToken)
	resp, err := a.hc.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: send: %s", redact(err.Error(), p.AccessToken))
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= 400 {
		return fmt.Errorf("whatsapp: send %d: %s", resp.StatusCode, redact(string(raw), p.AccessToken))
	}
	return nil
}

// ── parsing ─────────────────────────────────────────────────────────────────

type metaPayload struct {
	Entry []struct {
		Changes []struct {
			Value struct {
				Metadata struct {
					PhoneNumberID string `json:"phone_number_id"`
				} `json:"metadata"`
				Messages []struct {
					From string `json:"from"`
					ID   string `json:"id"`
					Type string `json:"type"`
					Text struct {
						Body string `json:"body"`
					} `json:"text"`
				} `json:"messages"`
			} `json:"value"`
		} `json:"changes"`
	} `json:"entry"`
}

func parseInbound(body []byte, p Provider) []adapter.Event {
	var mp metaPayload
	if json.Unmarshal(body, &mp) != nil {
		return nil
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	var out []adapter.Event
	for _, e := range mp.Entry {
		for _, c := range e.Changes {
			pnid := c.Value.Metadata.PhoneNumberID
			for _, m := range c.Value.Messages {
				out = append(out, adapter.Event{
					Provider: p.Name,
					Adapter:  "whatsapp",
					DedupKey: p.Name + ":" + m.ID,
					Source:   m.From,
					Message:  m.Text.Body,
					Payload:  raw,
					ReplyRef: map[string]any{"to": m.From, "phone_number_id": pnid, "provider": p.Name},
				})
			}
		}
	}
	return out
}

func verifyHMAC(secret string, body []byte, header string) bool {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	got := strings.TrimPrefix(header, "sha256=")
	return hmac.Equal([]byte(want), []byte(got))
}

func redact(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}
