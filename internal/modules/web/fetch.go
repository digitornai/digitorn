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
	URL        string        `json:"url"`
	Format     string        `json:"format"`
	Extract    flexjson.Bool `json:"extract"`
	Prompt     string        `json:"prompt"`
	Actions    []actionSpec  `json:"actions"`
	Screenshot     flexjson.Bool `json:"screenshot"`
	Live           flexjson.Bool `json:"live"`
	Crawl          *crawlSpec    `json:"crawl"`
	ApprovedSubmit flexjson.Bool `json:"approved_submit"`
	Profile        flexjson.Bool `json:"profile"`
}

// actionSpec is one interaction the agent asks the browser to perform on the
// page's live DOM before it is re-perceived. Elements are targeted by the ref
// the last perception stamped (data-dgn-ref).
type actionSpec struct {
	Do   string `json:"do"`   // click | type | press | select | upload | scroll | wait
	Ref  string `json:"ref"`  // target element ref (click/type/select/upload)
	Text string `json:"text"` // type: text to enter; select: option label to pick
	Key  string `json:"key"`  // press: enter|tab|escape|…
	Path string `json:"path"` // upload: file path inside the session workspace
	To   string `json:"to"`   // scroll: top|bottom (default: one viewport down)
	For  string `json:"for"`  // wait: networkidle | <css-selector>
	MS   int    `json:"ms"`   // wait: fixed milliseconds
}

// fetch retrieves a URL and returns its content as text/markdown/html. It is
// SSRF-guarded (dial + redirect), bounds the body read, caches raw HTML, and
// scans rendered content for prompt injection.
func (m *Module) fetch(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p fetchParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	// url is optional when actions are given: the agent acts on the page already
	// open in this session's live tab.
	var urlStr string
	if strings.TrimSpace(p.URL) != "" {
		target, err := normalizeURL(p.URL)
		if err != nil {
			return errResult(err), err
		}
		if err := m.checkDomain(target.Hostname()); err != nil {
			return errResult(err), err
		}
		urlStr = target.String()
	}

	format := p.Format
	switch format {
	case "text", "markdown", "html":
	default:
		format = "text"
	}

	cfg, client, _, cache := m.snapshot()

	sessionKey := "default"
	if id, ok := tool.IdentityFromContext(ctx); ok && id.SessionID != "" {
		sessionKey = id.SessionID
	}
	rt := defaultTimeout
	if cfg.FetchTimeoutSecs > 0 {
		rt = time.Duration(cfg.FetchTimeoutSecs * float64(time.Second))
	}
	wantShot := bool(p.Screenshot)
	wantLive := bool(p.Live)
	wantProfile := bool(p.Profile)

	// Crawl: bounded BFS over a site, returning a compact model per page.
	if p.Crawl != nil {
		if urlStr == "" {
			return errResult(fmt.Errorf("crawl requires a url to start from")), nil
		}
		return m.crawlSite(ctx, sessionKey, urlStr, *p.Crawl, cfg, client, rt), nil
	}

	var htmlDoc, screenshot, note string
	finalURL := urlStr
	cached := false

	switch {
	case len(p.Actions) > 0:
		// Interactive path: drive the session's persistent live tab. Navigate
		// first only when a url is given; otherwise act on the page already open
		// (state — cookies, scroll, JS — persists across calls).
		if m.browser == nil {
			return errResult(fmt.Errorf("browser engine unavailable for actions")), nil
		}
		if strings.TrimSpace(p.URL) != "" {
			if _, rerr := m.browser.navigate(ctx, sessionKey, urlStr, cfg.AllowPrivateHosts, rt, m.checkDomain, false, wantLive, wantProfile); rerr != nil {
				return errResult(rerr), nil
			}
		} else if !m.browser.hasTab(ctx, sessionKey, wantProfile) {
			// The agent wants to act but nothing is open in the live browser
			// (its refs came from a quick HTTP read). Open the last page so the
			// refs become live; re-perceive and hand them back rather than
			// clicking a stale ref.
			last, _ := m.lastURL.Load(sessionKey)
			lastU, _ := last.(string)
			if lastU == "" {
				return errResult(fmt.Errorf("no page is open in the live browser yet — fetch the url first, then act")), nil
			}
			if _, nerr := m.browser.navigate(ctx, sessionKey, lastU, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, wantProfile); nerr != nil {
				return errResult(nerr), nil
			}
			rr, _ := m.browser.act(ctx, sessionKey, nil, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, false, wantProfile)
			htmlDoc, finalURL, screenshot = rr.html, firstNonEmpty(rr.finalURL, lastU), rr.shot
			note = "Opened the page in the live browser. Your earlier refs came from a quick read and don't apply here — use the refs in this page model and resend your action."
			break
		}
		rr, rerr := m.browser.act(ctx, sessionKey, p.Actions, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, bool(p.ApprovedSubmit), wantProfile)
		if rerr != nil {
			// A stale ref (page re-rendered, or refs from a prior HTTP read):
			// re-perceive and return fresh refs with a note instead of a bare error.
			if isStaleRef(rerr) {
				if rr2, perr := m.browser.act(ctx, sessionKey, nil, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, false, wantProfile); perr == nil {
					htmlDoc, finalURL, screenshot = rr2.html, firstNonEmpty(rr2.finalURL, urlStr), rr2.shot
					note = "That ref didn't resolve (the page changed since you read it). Here is the current page with fresh refs — resend your action using these."
					break
				}
			}
			return errResult(rerr), nil
		}
		htmlDoc, finalURL, screenshot = rr.html, firstNonEmpty(rr.finalURL, urlStr), rr.shot

	case urlStr == "":
		// No url and no actions: re-perceive the page already open. Never
		// reloads, so a form the agent just filled survives. Clear error if
		// nothing is open in this session.
		if m.browser == nil {
			return errResult(fmt.Errorf("url is required (no page is open in this session yet)")), nil
		}
		rr, rerr := m.browser.act(ctx, sessionKey, nil, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, false, wantProfile)
		if rerr != nil {
			return errResult(rerr), nil
		}
		htmlDoc, finalURL, screenshot = rr.html, rr.finalURL, rr.shot

	default:
		// Read path: cache → HTTP → headless fallback. Screenshot/live bypass the
		// cache so a real browser tab actually runs.
		if !wantShot && !wantLive {
			htmlDoc, cached = cache.get(urlStr)
		}
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

			needBrowser := wantShot || wantLive // live needs a real tab to screencast
			if !needBrowser && m.browser != nil {
				visible := ""
				if probe := parseHTML(htmlDoc); probe != nil {
					visible = render(probe, false)
				}
				needBrowser = looksLikeJSShell(htmlDoc, visible)
			}
			if needBrowser && m.browser != nil {
				if rr, rerr := m.browser.navigate(ctx, sessionKey, finalURL, cfg.AllowPrivateHosts, rt, m.checkDomain, wantShot, wantLive, wantProfile); rerr == nil && strings.TrimSpace(rr.html) != "" {
					htmlDoc = rr.html
					finalURL = firstNonEmpty(rr.finalURL, finalURL)
					screenshot = rr.shot
				}
			}
			if !wantShot && !wantLive { // never cache screenshot/dynamic/live results
				cache.set(urlStr, htmlDoc)
			}
		}
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

	effectiveURL := firstNonEmpty(finalURL, urlStr) // acting on an open page has no urlStr
	data := map[string]any{
		"url":     effectiveURL,
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
		// Full page model: structure + navigation affordances (links = where to
		// go, actions = what to click) so the agent can navigate, not just read.
		if pm := buildPageModel(root, firstNonEmpty(finalURL, urlStr)); !pm.empty() {
			data["page"] = pm
		}
	}
	if finalURL != "" && urlStr != "" && finalURL != urlStr {
		data["final_url"] = finalURL
	}
	if screenshot != "" {
		data["screenshot"] = screenshot
	}
	if note != "" {
		data["note"] = note
	}
	if effectiveURL != "" {
		m.lastURL.Store(sessionKey, effectiveURL)
	}
	if m.detectInjection() {
		if w := scanForInjection(content); w != "" {
			data["security_warning"] = w
		}
	}

	return tool.Result{
		Success: true,
		Data:    data,
		Display: &tool.DisplayHint{Type: "markdown", Title: effectiveURL},
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

// isStaleRef reports whether an action failed because its ref no longer
// resolves — the cue to re-perceive and hand the agent fresh refs.
func isStaleRef(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found on current page")
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
