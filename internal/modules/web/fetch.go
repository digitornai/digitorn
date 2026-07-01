package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
	"github.com/digitornai/digitorn/internal/flexjson"
)

// fetchCache is a concurrency-safe, TTL-bounded, size-capped cache of fetched
// raw HTML keyed by final URL. It replaces the Python module's unsynchronized
// dict (a data race under the daemon's concurrent dispatch).
type fetchCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	maxSize int
	store   map[string]cacheEntry
}

type cacheEntry struct {
	at   time.Time
	html string
}

func newFetchCache(ttl time.Duration, maxSize int) *fetchCache {
	if maxSize <= 0 {
		maxSize = defaultCacheSize
	}
	return &fetchCache{ttl: ttl, maxSize: maxSize, store: make(map[string]cacheEntry, maxSize)}
}

func (c *fetchCache) get(key string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.store[key]
	if !ok {
		return "", false
	}
	if time.Since(e.at) > c.ttl {
		delete(c.store, key)
		return "", false
	}
	return e.html, true
}

func (c *fetchCache) set(key, html string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.store) >= c.maxSize {
		// Evict the oldest entry.
		var oldestKey string
		var oldestAt time.Time
		first := true
		for k, e := range c.store {
			if first || e.at.Before(oldestAt) {
				oldestKey, oldestAt, first = k, e.at, false
			}
		}
		delete(c.store, oldestKey)
	}
	c.store[key] = cacheEntry{at: time.Now(), html: html}
}

func (c *fetchCache) clear() {
	c.mu.Lock()
	c.store = make(map[string]cacheEntry, c.maxSize)
	c.mu.Unlock()
}

type fetchParams struct {
	URL     string   `json:"url"`
	Format  string   `json:"format"`
	Extract flexjson.Bool `json:"extract"`
	Prompt  string   `json:"prompt"`
}

// fetch retrieves a URL and returns its content as text/markdown/html. It is
// SSRF-guarded (dial + redirect), bounds the body read, caches raw HTML, and
// scans rendered content for prompt injection.
func (m *Module) fetch(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p fetchParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	target, err := normalizeURL(p.URL)
	if err != nil {
		return errResult(err), err
	}
	if err := m.checkDomain(target.Hostname()); err != nil {
		return errResult(err), err
	}

	format := p.Format
	switch format {
	case "text", "markdown", "html":
	default:
		format = "text"
	}

	cfg, client, _, cache := m.snapshot()
	urlStr := target.String()

	htmlDoc, cached := cache.get(urlStr)
	finalURL := urlStr
	if !cached {
		doc, ferr := m.download(ctx, client, urlStr)
		if ferr != nil {
			return errResult(ferr), ferr
		}
		if doc.binaryNote != "" {
			return tool.Result{Success: true, Data: map[string]any{
				"url":          urlStr,
				"content":      doc.binaryNote,
				"is_binary":    true,
				"content_type": doc.contentType,
			}}, nil
		}
		htmlDoc, finalURL = doc.body, doc.finalURL
		cache.set(urlStr, htmlDoc)
	}

	maxLen := cfg.MaxContentLength
	var content string
	root := parseHTML(htmlDoc)
	switch {
	case format == "html":
		content = truncateRunes(htmlDoc, maxLen)
	case root == nil:
		content = truncateRunes(htmlDoc, maxLen)
	default:
		base := root
		if p.Extract {
			base = mainContent(root)
		}
		content = truncateRunes(render(base, format == "markdown"), maxLen)
	}
	if p.Prompt != "" && format != "html" {
		content = applyPromptFilter(content, p.Prompt, maxLen)
	}

	data := map[string]any{
		"url":     urlStr,
		"content": content,
		"length":  len(content),
		"format":  format,
		"cached":  cached,
	}
	if root != nil {
		if t := extractTitle(root); t != "" {
			data["title"] = t
		}
		if d := extractMetaDescription(root); d != "" {
			data["description"] = d
		}
	}
	if finalURL != "" && finalURL != urlStr {
		data["final_url"] = finalURL
	}
	if m.detectInjection() {
		if w := scanForInjection(content); w != "" {
			data["security_warning"] = w
		}
	}

	return tool.Result{
		Success: true,
		Data:    data,
		Display: &tool.DisplayHint{Type: "markdown", Title: urlStr},
	}, nil
}

// fetchedDoc is the outcome of a single body retrieval.
type fetchedDoc struct {
	body        string
	finalURL    string
	contentType string
	binaryNote  string // set instead of body when the content is not renderable text
}

// download performs the GET and reads a bounded, decoded body. It classifies
// non-text content into a short note instead of returning raw bytes.
func (m *Module) download(ctx context.Context, client *http.Client, urlStr string) (fetchedDoc, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
	if err != nil {
		return fetchedDoc{}, err
	}
	cfg, _, _, _ := m.snapshot()
	req.Header.Set("User-Agent", cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")

	resp, err := client.Do(req)
	if err != nil {
		return fetchedDoc{}, fmt.Errorf("fetch %s: %w", urlStr, err)
	}
	defer resp.Body.Close()

	// Redirects are already followed by the client; treat any 2xx as success
	// and only 4xx/5xx as an error the agent should see.
	if resp.StatusCode >= 400 {
		return fetchedDoc{}, fmt.Errorf("HTTP %d fetching %s", resp.StatusCode, urlStr)
	}

	finalURL := resp.Request.URL.String()
	ctype := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(ctype)
	if !isTextLike(mediaType) {
		// Read nothing further; report a short note. We trust no Content-Length.
		return fetchedDoc{
			finalURL:    finalURL,
			contentType: ctype,
			binaryNote:  fmt.Sprintf("[non-text content: %s — cannot render as text]", firstNonEmpty(mediaType, ctype, "unknown")),
		}, nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFetchBytes))
	if err != nil {
		return fetchedDoc{}, fmt.Errorf("read body %s: %w", urlStr, err)
	}
	return fetchedDoc{body: string(body), finalURL: finalURL, contentType: ctype}, nil
}

// isTextLike reports whether a media type carries renderable text.
func isTextLike(mediaType string) bool {
	if mediaType == "" {
		return true // unknown: attempt to render
	}
	if strings.HasPrefix(mediaType, "text/") {
		return true
	}
	switch mediaType {
	case "application/xhtml+xml", "application/xml", "application/json",
		"application/rss+xml", "application/atom+xml", "application/ld+json":
		return true
	}
	return false
}

// normalizeURL trims, defaults the scheme to https, and rejects anything that
// is not an absolute http(s) URL with a host.
func normalizeURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("url must not be empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported url scheme %q (only http/https)", u.Scheme)
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("url has no host")
	}
	return u, nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
