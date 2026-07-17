package credentials

import (
	"context"
	"sync"
	"time"
)

type Resolver struct {
	store *Store
	mu    sync.RWMutex
	cache map[string]map[string]resolved
}

type resolved struct {
	apiKey    string
	baseURL   string
	expiresAt time.Time
}

func (c resolved) expired() bool {
	return !c.expiresAt.IsZero() && time.Now().After(c.expiresAt)
}

const copilotSessionTTL = 20 * time.Minute

func NewResolver(store *Store) *Resolver {
	return &Resolver{store: store, cache: map[string]map[string]resolved{}}
}

func (r *Resolver) Lookup(ctx context.Context, userID, provider string) (apiKey, baseURL string, ok bool) {
	if r == nil || r.store == nil || userID == "" || provider == "" {
		return "", "", false
	}

	r.mu.RLock()
	if byProvider, hit := r.cache[userID]; hit {
		if c, ok2 := byProvider[provider]; ok2 && !c.expired() {
			r.mu.RUnlock()
			return c.apiKey, c.baseURL, c.apiKey != "" || c.baseURL != ""
		}
	}
	r.mu.RUnlock()

	c := resolved{}
	if _, fields, err := r.store.revealForProvider(ctx, userID, provider, ""); err == nil {
		c.apiKey = fields["api_key"]
		c.baseURL = fields["base_url"]
		if provider == copilotProvider {
			if tok, err := r.store.exchangeCopilotToken(ctx, c.apiKey); err == nil && tok != "" {
				c = resolved{apiKey: tok, baseURL: copilotAPIBase, expiresAt: time.Now().Add(copilotSessionTTL)}
			} else {
				c = resolved{}
			}
		}
	}

	r.mu.Lock()
	if r.cache[userID] == nil {
		r.cache[userID] = map[string]resolved{}
	}
	r.cache[userID][provider] = c
	r.mu.Unlock()

	return c.apiKey, c.baseURL, c.apiKey != "" || c.baseURL != ""
}

func (r *Resolver) Invalidate(userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.cache, userID)
	r.mu.Unlock()
}
