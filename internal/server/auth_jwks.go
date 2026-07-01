package server

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

// JWKSConfig configures a JWKS client. JWKSURL takes precedence; otherwise
// OIDC discovery is attempted at `{Issuer}/.well-known/openid-configuration`,
// falling back to `{Issuer}/.well-known/jwks.json`.
type JWKSConfig struct {
	Issuer          string
	JWKSURL         string
	RefreshInterval time.Duration
	FailureBackoff  time.Duration
	HTTPClient      *http.Client
	Logger          *slog.Logger
}

// JWKS is a scalable, lock-free JWKS cache. Readers never block — the
// current keyset is stored behind an atomic.Pointer and replaced wholesale
// at each refresh. Designed to absorb millions of GetKey() calls/s.
type JWKS struct {
	cfg JWKSConfig

	current atomic.Pointer[jwksSet]

	mu           sync.Mutex // refresh-only
	resolvedURL  string
	lastFailUnix atomic.Int64
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	started      atomic.Bool

	refreshes atomic.Uint64
	failures  atomic.Uint64
	missKid   atomic.Uint64
}

type jwksSet struct {
	keysByKID map[string]*rsa.PublicKey
	fallback  *rsa.PublicKey
	loadedAt  time.Time
	raw       []byte
}

// jwkKey is the on-the-wire representation of one key in a JWKS document.
// We only need the RSA fields and `kid`/`alg`/`use` for indexing.
type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type jwksDoc struct {
	Keys []jwkKey `json:"keys"`
}

type oidcDoc struct {
	JWKSURI string `json:"jwks_uri"`
}

var (
	ErrJWKSNotStarted = errors.New("jwks: not started")
	ErrJWKSEmpty      = errors.New("jwks: empty keyset")
	ErrJWKSKidMissing = errors.New("jwks: kid not found")
)

// NewJWKS constructs the JWKS client. Start must be called before GetKey.
func NewJWKS(cfg JWKSConfig) *JWKS {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.RefreshInterval <= 0 {
		cfg.RefreshInterval = 24 * time.Hour
	}
	if cfg.FailureBackoff <= 0 {
		cfg.FailureBackoff = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &JWKS{cfg: cfg}
}

// Start performs OIDC discovery and the initial JWKS fetch, then spawns a
// background refresher. Returns an error if the initial fetch fails.
func (j *JWKS) Start(ctx context.Context) error {
	if !j.started.CompareAndSwap(false, true) {
		return nil
	}
	j.ctx, j.cancel = context.WithCancel(ctx)
	if err := j.discoverAndFetch(j.ctx); err != nil {
		j.started.Store(false)
		j.cancel()
		return fmt.Errorf("jwks initial fetch: %w", err)
	}
	j.wg.Add(1)
	go j.refreshLoop()
	return nil
}

// Stop drains the refresher goroutine.
func (j *JWKS) Stop(ctx context.Context) error {
	if !j.started.Load() {
		return nil
	}
	j.cancel()
	done := make(chan struct{})
	go func() { j.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GetKey returns the RSA public key for a given kid. If absent, it forces
// one refresh (subject to negative cache backoff) and retries.
func (j *JWKS) GetKey(kid string) (*rsa.PublicKey, error) {
	set := j.current.Load()
	if set == nil {
		return nil, ErrJWKSNotStarted
	}
	if kid != "" {
		if k, ok := set.keysByKID[kid]; ok {
			return k, nil
		}
	}
	if set.fallback != nil && kid == "" {
		return set.fallback, nil
	}
	j.missKid.Add(1)
	// Force-refresh once, respecting negative-cache backoff.
	if j.allowForceRefresh() {
		_ = j.refreshOnce(j.ctx)
		set = j.current.Load()
		if k, ok := set.keysByKID[kid]; ok {
			return k, nil
		}
	}
	if kid == "" && set.fallback != nil {
		return set.fallback, nil
	}
	return nil, ErrJWKSKidMissing
}

// KeyCount returns the number of keys currently cached (atomic snapshot).
func (j *JWKS) KeyCount() int {
	set := j.current.Load()
	if set == nil {
		return 0
	}
	return len(set.keysByKID)
}

// Stats returns observability counters.
type JWKSStats struct {
	Refreshes uint64
	Failures  uint64
	MissKid   uint64
	Keys      int
	LoadedAt  time.Time
}

func (j *JWKS) Stats() JWKSStats {
	set := j.current.Load()
	loadedAt := time.Time{}
	keys := 0
	if set != nil {
		loadedAt = set.loadedAt
		keys = len(set.keysByKID)
	}
	return JWKSStats{
		Refreshes: j.refreshes.Load(),
		Failures:  j.failures.Load(),
		MissKid:   j.missKid.Load(),
		Keys:      keys,
		LoadedAt:  loadedAt,
	}
}

func (j *JWKS) refreshLoop() {
	defer j.wg.Done()
	t := time.NewTicker(j.cfg.RefreshInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			safego.Run("server.jwks.refresh", func() { _ = j.refreshOnce(j.ctx) })
		case <-j.ctx.Done():
			return
		}
	}
}

func (j *JWKS) allowForceRefresh() bool {
	last := j.lastFailUnix.Load()
	if last == 0 {
		return true
	}
	return time.Since(time.Unix(last, 0)) >= j.cfg.FailureBackoff
}

func (j *JWKS) discoverAndFetch(ctx context.Context) error {
	j.mu.Lock()
	if j.resolvedURL == "" {
		j.resolvedURL = j.cfg.JWKSURL
	}
	if j.resolvedURL == "" {
		// Try OIDC discovery.
		if j.cfg.Issuer == "" {
			j.mu.Unlock()
			return errors.New("jwks: issuer and jwks_url both empty")
		}
		issuer := strings.TrimRight(j.cfg.Issuer, "/")
		u := issuer + "/.well-known/openid-configuration"
		if uri, err := j.fetchOIDCJWKSURI(ctx, u); err == nil && uri != "" {
			j.resolvedURL = uri
			j.cfg.Logger.Info("jwks: OIDC discovery", slog.String("jwks_uri", uri))
		} else {
			j.resolvedURL = issuer + "/.well-known/jwks.json"
			j.cfg.Logger.Info("jwks: discovery fallback", slog.String("jwks_uri", j.resolvedURL))
		}
	}
	j.mu.Unlock()
	return j.refreshOnce(ctx)
}

func (j *JWKS) fetchOIDCJWKSURI(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := j.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oidc discovery: status %d", resp.StatusCode)
	}
	var doc oidcDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	return doc.JWKSURI, nil
}

func (j *JWKS) refreshOnce(ctx context.Context) error {
	j.mu.Lock()
	url := j.resolvedURL
	j.mu.Unlock()
	if url == "" {
		return errors.New("jwks: unresolved URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		j.recordFailure()
		return err
	}
	resp, err := j.cfg.HTTPClient.Do(req)
	if err != nil {
		j.recordFailure()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		j.recordFailure()
		return fmt.Errorf("jwks: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		j.recordFailure()
		return err
	}
	set, err := parseJWKS(raw)
	if err != nil {
		j.recordFailure()
		return fmt.Errorf("jwks parse: %w", err)
	}
	set.loadedAt = time.Now()
	j.current.Store(set)
	j.refreshes.Add(1)
	j.lastFailUnix.Store(0)
	j.cfg.Logger.Info("jwks: refreshed",
		slog.Int("keys", len(set.keysByKID)),
		slog.String("url", url))
	return nil
}

func (j *JWKS) recordFailure() {
	j.failures.Add(1)
	j.lastFailUnix.Store(time.Now().Unix())
}

func parseJWKS(raw []byte) (*jwksSet, error) {
	var doc jwksDoc
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Keys) == 0 {
		return nil, ErrJWKSEmpty
	}
	out := &jwksSet{
		keysByKID: make(map[string]*rsa.PublicKey, len(doc.Keys)),
		raw:       raw,
	}
	var usable int
	var only *rsa.PublicKey
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := decodeRSA(k.N, k.E)
		if err != nil {
			continue
		}
		usable++
		only = pub
		if k.Kid != "" {
			out.keysByKID[k.Kid] = pub
		}
	}
	// A kid-less fallback is only safe when the set publishes exactly one key.
	// With several keys, a token must name its kid — otherwise an unkeyed token
	// could be validated against an unintended key and key rotation is moot.
	if usable == 1 {
		out.fallback = only
	}
	if len(out.keysByKID) == 0 && out.fallback == nil {
		return nil, ErrJWKSEmpty
	}
	return out, nil
}

func decodeRSA(nB64, eB64 string) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		// Some JWKS use base64 standard padded; try fallback.
		nBytes, err = base64.URLEncoding.DecodeString(nB64)
		if err != nil {
			return nil, fmt.Errorf("n: %w", err)
		}
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil {
		eBytes, err = base64.URLEncoding.DecodeString(eB64)
		if err != nil {
			return nil, fmt.Errorf("e: %w", err)
		}
	}
	n := new(big.Int).SetBytes(nBytes)
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, errors.New("rsa: exponent zero")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}
