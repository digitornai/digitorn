package credentials

import (
	"context"
	"sync"
)

// Resolver is the RUNTIME side of the vault: a non-blocking, in-memory cache of
// a user's resolved provider credentials, read on the agent hot path.
//
// The settings-plane Store does the DB + crypto; the Resolver decrypts a
// credential ONCE per (user, provider) and caches the plaintext apiKey/baseURL.
// A turn's Lookup is then an O(1) map read — never a DB read or a decryption per
// LLM call. A CRUD mutation calls Invalidate so the next turn re-reads. The
// engine only ever consults this in BYOK mode; otherwise the gateway is used.
type Resolver struct {
	store *Store
	mu    sync.RWMutex
	cache map[string]map[string]resolved // userID -> providerName -> creds
}

type resolved struct {
	apiKey  string
	baseURL string
}

func NewResolver(store *Store) *Resolver {
	return &Resolver{store: store, cache: map[string]map[string]resolved{}}
}

// Lookup returns the user's stored apiKey/baseURL for a provider. ok is false
// when the user has no usable key for that provider (the caller then falls back
// to the bundle credential / gateway). The first lookup per (user, provider)
// reads + decrypts once — fast, local sqlite + NaCl, no network; every
// subsequent lookup is an O(1) cache hit. The "absent" verdict is cached too,
// so a provider the user hasn't set never re-hits the DB on later turns.
func (r *Resolver) Lookup(ctx context.Context, userID, provider string) (apiKey, baseURL string, ok bool) {
	if r == nil || r.store == nil || userID == "" || provider == "" {
		return "", "", false
	}

	r.mu.RLock()
	if byProvider, hit := r.cache[userID]; hit {
		if c, ok2 := byProvider[provider]; ok2 {
			r.mu.RUnlock()
			return c.apiKey, c.baseURL, c.apiKey != ""
		}
	}
	r.mu.RUnlock()

	// Cold: resolve once and cache (including the empty verdict).
	c := resolved{}
	if _, fields, err := r.store.revealForProvider(ctx, userID, provider, ""); err == nil {
		c.apiKey = fields["api_key"]
		c.baseURL = fields["base_url"]
	}

	r.mu.Lock()
	if r.cache[userID] == nil {
		r.cache[userID] = map[string]resolved{}
	}
	r.cache[userID][provider] = c
	r.mu.Unlock()

	return c.apiKey, c.baseURL, c.apiKey != ""
}

// Invalidate drops a user's cached credentials so the next Lookup re-reads.
// Called after any CRUD mutation on that user's vault.
func (r *Resolver) Invalidate(userID string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	delete(r.cache, userID)
	r.mu.Unlock()
}
