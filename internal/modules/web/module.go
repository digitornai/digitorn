// Package web exposes two LLM-facing tools — search and fetch — for finding
// and reading web content. Every outbound request an agent can trigger is
// vetted by an SSRF guard (private/loopback/link-local addresses are refused,
// each redirect hop is re-checked), response bodies are size-bounded, and an
// optional domain allow/block policy is enforced. fetch folds content
// extraction in via its extract mode; there is no separate extract tool.
package web

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	domainmodule "github.com/digitornai/digitorn/internal/domain/module"
	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/pkg/module"
)

const (
	defaultUserAgent  = "Mozilla/5.0 (compatible; Digitorn/1.0; +https://digitorn.dev)"
	defaultMaxContent = 50000
	defaultCacheTTL   = 900 * time.Second
	defaultTimeout    = 30 * time.Second
	defaultCacheSize  = 100
	// maxFetchBytes caps how much of a response body is ever read into memory,
	// independent of the (smaller) rendered-content limit, so a hostile or
	// runaway page cannot exhaust memory.
	maxFetchBytes = 12 << 20 // 12 MiB
)

// Config is the per-app configuration for the web module.
type Config struct {
	SearchBackend     string            `json:"search_backend" yaml:"search_backend"`
	SearchFallback    string            `json:"search_fallback" yaml:"search_fallback"`
	UserAgent         string            `json:"user_agent" yaml:"user_agent"`
	CacheTTLSeconds   float64           `json:"cache_ttl" yaml:"cache_ttl"`
	MaxContentLength  int               `json:"max_content_length" yaml:"max_content_length"`
	FetchTimeoutSecs  float64           `json:"fetch_timeout" yaml:"fetch_timeout"`
	AllowedDomains    []string          `json:"allowed_domains" yaml:"allowed_domains"`
	BlockedDomains    []string          `json:"blocked_domains" yaml:"blocked_domains"`
	AllowPrivateHosts bool              `json:"allow_private_hosts" yaml:"allow_private_hosts"`
	DetectInjection   *bool             `json:"detect_injection" yaml:"detect_injection"`
	APIKeys           map[string]string `json:"api_keys" yaml:"api_keys"`
}

// Module is the web module instance. All mutable state is guarded by mu so a
// hot-reload (UpdateConfig) can swap the client/cache while requests run.
type Module struct {
	module.Base

	mu      sync.RWMutex
	cfg     Config
	client  *http.Client // SSRF-guarded; used for LLM-supplied URLs (fetch)
	backend *http.Client // plain; used for operator-configured search endpoints
	cache   *fetchCache
}

// New constructs the web module with its tools wired.
func New() *Module {
	m := &Module{}
	m.Base = module.Base{
		ID:          "web",
		Version:     "1.0.0",
		Description: "Search the web and fetch pages as clean text/markdown.",
		SupportedPlatforms: []domainmodule.Platform{
			domainmodule.PlatformLinux,
			domainmodule.PlatformMacOS,
			domainmodule.PlatformWindows,
		},
		Constraints: []module.ConstraintSpec{
			{Name: "allowed_domains", Type: "string_list", Description: "Restrict web fetch/search to these domains.", Scope: "universal"},
			{Name: "blocked_domains", Type: "string_list", Description: "Block these domains from fetch/search.", Scope: "universal"},
		},
	}

	m.RegisterTool(module.Tool{
		Name:        "search",
		Description: "Search the web and return ranked results. Each result carries title, URL, snippet AND (by default) the page's main content rendered as clean Markdown — headings, links and lists preserved — so you can usually answer without a separate fetch. Set fetch_content=false for snippet-only.",
		Params: []tool.ParamSpec{
			{Name: "query", Type: "string", Description: "Search query. Be specific (include version/year for current info).", Required: true},
			{Name: "limit", Type: "integer", Description: "Max results (1-25, default 10).", Default: 10},
			{Name: "fetch_content", Type: "boolean", Description: "Also return each result's main page text inline (default true) so you can answer without a separate fetch. Set false for a fast snippet-only search.", Default: true},
			{Name: "time_range", Type: "string", Description: "Restrict to recent results.", Enum: []any{"day", "week", "month", "year"}},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"web", "search"},
		Aliases:   []string{"search", "web search", "rechercher"},
		CLILabel:  "Search",
		CLIParam:  "query",
		Handler:   m.search,
	})

	m.RegisterTool(module.Tool{
		Name: "fetch",
		Description: "Fetch a web page and return its content as clean text (default), markdown, or raw html. " +
			"Set extract=true to keep only the main article body (strips nav/ads/footer). " +
			"Set prompt to focus the returned content on a topic. Results are cached briefly.",
		Params: []tool.ParamSpec{
			{Name: "url", Type: "string", Description: "URL to fetch.", Required: true},
			{Name: "format", Type: "string", Description: "Output format.", Default: "text", Enum: []any{"text", "markdown", "html"}},
			{Name: "extract", Type: "boolean", Description: "Return only the main content (article body).", Default: false},
			{Name: "prompt", Type: "string", Description: "Optional topic to focus extraction on; leave empty for the full page."},
		},
		RiskLevel: tool.RiskLow,
		Tags:      []string{"web", "fetch", "read"},
		Aliases:   []string{"fetch", "fetch url", "open url", "lire page"},
		CLILabel:  "Fetch",
		CLIParam:  "url",
		Handler:   m.fetch,
	})

	return m
}

// Init binds config, applies defaults and builds the HTTP clients + cache.
func (m *Module) Init(ctx context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	m.apply(c)
	return nil
}

// UpdateConfig rebuilds the clients/cache atomically on hot-reload.
func (m *Module) UpdateConfig(ctx context.Context, cfg map[string]any) error {
	var c Config
	if err := m.BindConfig(cfg, &c); err != nil {
		return err
	}
	m.apply(c)
	return nil
}

// apply normalizes a config and swaps the live clients/cache under the lock.
func (m *Module) apply(c Config) {
	if c.SearchBackend == "" {
		c.SearchBackend = "duckduckgo"
	}
	if c.UserAgent == "" {
		c.UserAgent = defaultUserAgent
	}
	if c.MaxContentLength <= 0 {
		c.MaxContentLength = defaultMaxContent
	}
	ttl := defaultCacheTTL
	if c.CacheTTLSeconds > 0 {
		ttl = time.Duration(c.CacheTTLSeconds * float64(time.Second))
	}
	timeout := defaultTimeout
	if c.FetchTimeoutSecs > 0 {
		timeout = time.Duration(c.FetchTimeoutSecs * float64(time.Second))
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.cfg = c
	m.client = newGuardedClient(timeout, c.AllowPrivateHosts, m.redirectGuard)
	m.backend = newGuardedClient(searchTimeout, true, nil) // operator endpoints are trusted
	m.cache = newFetchCache(ttl, defaultCacheSize)
}

// Stop releases idle connections and clears the cache.
func (m *Module) Stop(ctx context.Context) error {
	m.mu.Lock()
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
	if m.backend != nil {
		m.backend.CloseIdleConnections()
	}
	if m.cache != nil {
		m.cache.clear()
	}
	m.mu.Unlock()
	return m.Base.Stop(ctx)
}

// snapshot returns the live config + clients under a read lock so handlers
// never touch fields a concurrent reload is swapping.
func (m *Module) snapshot() (Config, *http.Client, *http.Client, *fetchCache) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg, m.client, m.backend, m.cache
}

// detectInjection reports whether injection scanning is on (default true).
func (m *Module) detectInjection() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.DetectInjection == nil || *m.cfg.DetectInjection
}

// redirectGuard re-applies the domain policy on every redirect hop and caps
// the chain length. The IP-level SSRF check runs in the dialer per hop.
func (m *Module) redirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return fmt.Errorf("stopped after 10 redirects")
	}
	return m.checkDomain(req.URL.Hostname())
}

// checkDomain enforces the block list then the allow list against host. An
// empty allow list means "any host not blocked".
func (m *Module) checkDomain(host string) error {
	if host == "" {
		return fmt.Errorf("invalid or missing host")
	}
	m.mu.RLock()
	allowed := m.cfg.AllowedDomains
	blocked := m.cfg.BlockedDomains
	m.mu.RUnlock()
	for _, b := range blocked {
		if domainMatches(host, b) {
			return fmt.Errorf("domain %q is blocked by egress policy", host)
		}
	}
	if len(allowed) > 0 {
		for _, a := range allowed {
			if domainMatches(host, a) {
				return nil
			}
		}
		return fmt.Errorf("domain %q is not in the allowed list", host)
	}
	return nil
}

// domainMatches reports whether host equals domain or is a subdomain of it.
func domainMatches(host, domain string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSuffix(domain, "."), "."))
	return host == domain || strings.HasSuffix(host, "."+domain)
}

// truncateRunes cuts s to at most max bytes without splitting a UTF-8 rune,
// appending a truncation note when it actually trimmed.
func truncateRunes(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("\n\n... (truncated at %d chars)", cut)
}

// applyPromptFilter keeps the paragraphs most relevant to prompt (by keyword
// hit count), preserving document order within the budget; returns the head of
// content when nothing matches.
func applyPromptFilter(content, prompt string, max int) string {
	var keywords []string
	for _, w := range strings.Fields(strings.ToLower(prompt)) {
		if len(w) > 3 {
			keywords = append(keywords, w)
		}
	}
	if len(keywords) == 0 {
		return truncateRunes(content, max)
	}
	type scored struct {
		idx, score int
		text       string
	}
	var hits []scored
	for i, para := range strings.Split(content, "\n\n") {
		lower := strings.ToLower(para)
		s := 0
		for _, kw := range keywords {
			if strings.Contains(lower, kw) {
				s++
			}
		}
		if s > 0 {
			hits = append(hits, scored{idx: i, score: s, text: para})
		}
	}
	if len(hits) == 0 {
		return truncateRunes(content, max)
	}
	// Highest score first; stable for ties (insertion sort).
	for i := 1; i < len(hits); i++ {
		for j := i; j > 0 && hits[j-1].score < hits[j].score; j-- {
			hits[j-1], hits[j] = hits[j], hits[j-1]
		}
	}
	var picked []scored
	total := 0
	for _, h := range hits {
		if total+len(h.text) > max {
			break
		}
		picked = append(picked, h)
		total += len(h.text) + 2
	}
	// Restore document order for readability.
	for i := 1; i < len(picked); i++ {
		for j := i; j > 0 && picked[j-1].idx > picked[j].idx; j-- {
			picked[j-1], picked[j] = picked[j], picked[j-1]
		}
	}
	parts := make([]string, len(picked))
	for i, p := range picked {
		parts[i] = p.text
	}
	return strings.Join(parts, "\n\n")
}

func errResult(err error) tool.Result {
	return tool.Result{Success: false, Error: err.Error()}
}
