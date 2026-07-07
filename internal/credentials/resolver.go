package credentials

import (
	"context"
	"sync"
	"time"
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
	// expiresAt bounds short-lived brokered tokens (e.g. a Copilot session
	// token). Zero = the credential doesn't expire (plain api_key/base_url).
	expiresAt time.Time
}

func (c resolved) expired() bool {
	return !c.expiresAt.IsZero() && time.Now().After(c.expiresAt)
}

// copilotSessionTTL keeps an exchanged Copilot token comfortably within its
// ~30 min validity while amortizing the exchange across turns.
const copilotSessionTTL = 20 * time.Minute

func NewResolver(store *Store) *Resolver {
	return &Resolver{store: store, cache: map[string]map[string]resolved{}}
}

// Lookup returns the user's stored apiKey/baseURL for a provider. ok is false
// when the user has no usable key for that provider (the caller then falls back
// to the bundle credential / gateway). The first lookup per (user, provider)
// reads + decrypts once — fast, local sqlite + NaCl, no network; every
// subsequent lookup is an O(1) cache hit. The "absent" verdict is cached too,
// so a provider the user hasn't set never re-hits the DB on later turns.
//
// Brokered providers (Copilot) resolve to their SESSION token + API base — the
// exchange happens here, cached with a TTL, so the turn path stays O(1) between
// refreshes and downstream treats the result like any OpenAI-compatible remote.
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

	// Cold (or expired): resolve once and cache (including the empty verdict).
	c := resolved{}
	if _, fields, err := r.store.revealForProvider(ctx, userID, provider, ""); err == nil {
		c.apiKey = fields["api_key"]
		c.baseURL = fields["base_url"]
		// Brokered flow: the stored api_key is a GitHub OAuth token, not an LLM
		// key — exchange it for the short-lived Copilot session token and point
		// at the Copilot API. On exchange failure resolve to "absent" (the turn
		// falls back / errors explicitly rather than sending a GitHub token to
		// the wrong host).
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
