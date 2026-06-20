package web

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbathepaul/digitorn/internal/domain/tool"
	"github.com/mbathepaul/digitorn/internal/flexjson"
)

const (
	searchTimeout  = 20 * time.Second
	maxSearchBytes = 4 << 20 // 4 MiB of search-result HTML/JSON is plenty
	defaultLimit   = 10
	maxLimit       = 25
	ddgEndpoint    = "https://html.duckduckgo.com/html/"
	braveEndpoint  = "https://api.search.brave.com/res/v1/web/search"
	tavilyEndpoint = "https://api.tavily.com/search"
	googleEndpoint = "https://www.googleapis.com/customsearch/v1"
	searxngDefault = "http://localhost:8080"
)

type searchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Content string `json:"content,omitempty"` // main page text, filled when fetch_content is on
}

type searchParams struct {
	Query        string    `json:"query"`
	Limit        flexjson.Int   `json:"limit"`
	FetchContent *flexjson.Bool `json:"fetch_content"`
	TimeRange    string    `json:"time_range"`
}

// searchOpts carries per-call knobs each backend maps as it can.
type searchOpts struct {
	timeRange string
}

const (
	// maxInlineContentChars caps the main text attached to each result so a
	// content-enriched search stays token-affordable.
	maxInlineContentChars = 4000
	// enrichWorkers bounds the concurrent top-k fetches done to fill content.
	enrichWorkers = 5
)

// search runs the configured backend (with optional fallback), filters results
// through the domain policy, and returns title/url/snippet rows.
func (m *Module) search(ctx context.Context, raw json.RawMessage) (tool.Result, error) {
	var p searchParams
	if err := json.Unmarshal(raw, &p); err != nil {
		return errResult(err), err
	}
	p.Query = strings.TrimSpace(p.Query)
	if len(p.Query) < 2 {
		err := fmt.Errorf("query must be at least 2 characters")
		return errResult(err), err
	}
	limit := int(p.Limit)
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	opts := searchOpts{timeRange: normalizeTimeRange(p.TimeRange)}
	cfg, client, backend, _ := m.snapshot()
	primary := cfg.SearchBackend

	results, err := m.runBackend(ctx, backend, cfg, primary, p.Query, limit, opts)
	usedBackend := primary
	note := ""
	if err != nil && cfg.SearchFallback != "" && cfg.SearchFallback != primary {
		var ferr error
		results, ferr = m.runBackend(ctx, backend, cfg, cfg.SearchFallback, p.Query, limit, opts)
		if ferr != nil {
			e := fmt.Errorf("search failed on %q (%v) and fallback %q (%v)", primary, err, cfg.SearchFallback, ferr)
			return errResult(e), e
		}
		usedBackend = cfg.SearchFallback
		note = fmt.Sprintf("primary backend %q failed, used fallback", primary)
	} else if err != nil {
		e := fmt.Errorf("search failed (%s): %w", primary, err)
		return errResult(e), e
	}

	results = dedupByURL(m.filterByDomain(results))
	if len(results) > limit {
		results = results[:limit]
	}

	// Enrich with inline page content (our free equivalent of Tavily's
	// content-bearing results): fetch the top-k in parallel and attach the
	// extracted main text. Best-effort — a result whose page can't be fetched
	// simply keeps its snippet.
	if p.FetchContent == nil || bool(*p.FetchContent) {
		m.enrichWithContent(ctx, client, results)
	}

	sources := make([]string, 0, len(results))
	for _, r := range results {
		if r.URL != "" {
			sources = append(sources, r.URL)
		}
	}

	data := map[string]any{
		"query":   p.Query,
		"results": results,
		"count":   len(results),
		"backend": usedBackend,
		"sources": sources,
	}
	if note != "" {
		data["note"] = note
	}
	return tool.Result{Success: true, Data: data, Display: &tool.DisplayHint{Type: "json", Title: "Search: " + p.Query}}, nil
}

// enrichWithContent fetches each result's page in parallel (bounded) and fills
// its Content with the extracted main text. Each goroutine writes a distinct
// slice index, so no lock is needed; failures leave Content empty.
func (m *Module) enrichWithContent(ctx context.Context, client *http.Client, results []searchResult) {
	if client == nil {
		return
	}
	sem := make(chan struct{}, enrichWorkers)
	var wg sync.WaitGroup
	for i := range results {
		if results[i].URL == "" || results[i].Content != "" {
			continue // skip empties and backends (Tavily) that already returned content
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() {
				<-sem
				_ = recover() // a single bad page must never crash the search
			}()
			results[i].Content = m.extractMainMarkdown(ctx, client, results[i].URL, maxInlineContentChars)
		}(i)
	}
	wg.Wait()
}

// extractMainMarkdown fetches url and returns its main content rendered as
// Markdown — headings (#), links ([text](url)), lists and code blocks are
// preserved so the agent gets the page's structure, not flattened text —
// truncated to maxChars. Returns "" on any error or non-text content.
func (m *Module) extractMainMarkdown(ctx context.Context, client *http.Client, url string, maxChars int) string {
	doc, err := m.download(ctx, client, url)
	if err != nil || doc.binaryNote != "" || doc.body == "" {
		return ""
	}
	root := parseHTML(doc.body)
	if root == nil {
		return ""
	}
	return truncateRunes(render(mainContent(root), true), maxChars)
}

// dedupByURL removes duplicate and empty-URL results, preserving order.
func dedupByURL(in []searchResult) []searchResult {
	seen := make(map[string]bool, len(in))
	out := in[:0:0]
	for _, r := range in {
		if r.URL == "" || seen[r.URL] {
			continue
		}
		seen[r.URL] = true
		out = append(out, r)
	}
	return out
}

// normalizeTimeRange validates a recency window, returning "" for anything
// unrecognized (no filter).
func normalizeTimeRange(tr string) string {
	switch strings.ToLower(strings.TrimSpace(tr)) {
	case "day", "week", "month", "year":
		return strings.ToLower(strings.TrimSpace(tr))
	default:
		return ""
	}
}

// filterByDomain drops results the domain policy forbids.
func (m *Module) filterByDomain(in []searchResult) []searchResult {
	out := in[:0:0]
	for _, r := range in {
		if m.checkDomain(hostOf(r.URL)) == nil {
			out = append(out, r)
		}
	}
	return out
}

func (m *Module) runBackend(ctx context.Context, client *http.Client, cfg Config, backend, query string, limit int, opts searchOpts) ([]searchResult, error) {
	switch backend {
	case "duckduckgo", "":
		return searchDuckDuckGo(ctx, client, cfg.UserAgent, query, limit, opts)
	case "searxng":
		return searchSearxng(ctx, client, cfg, query, limit, opts)
	case "brave":
		return searchBrave(ctx, client, cfg, query, limit, opts)
	case "tavily":
		return searchTavily(ctx, client, cfg, query, limit, opts)
	case "google":
		return searchGoogle(ctx, client, cfg, query, limit, opts)
	default:
		return nil, fmt.Errorf("unknown search backend %q", backend)
	}
}

// readJSON does a request and decodes a bounded JSON body into v.
func readJSON(resp *http.Response, v any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxSearchBytes)).Decode(v)
}

func searchDuckDuckGo(ctx context.Context, client *http.Client, ua, query string, limit int, opts searchOpts) ([]searchResult, error) {
	const maxRetries = 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(attempt) * 800 * time.Millisecond):
			}
		}
		results, err := doDuckDuckGoRequest(ctx, client, ua, query, limit, opts)
		if err == nil && len(results) > 0 {
			return results, nil
		}
		lastErr = err
		if err == nil {
			lastErr = fmt.Errorf("no results returned (possible anti-bot block)")
		}
	}
	return nil, lastErr
}

func doDuckDuckGoRequest(ctx context.Context, client *http.Client, ua, query string, limit int, opts searchOpts) ([]searchResult, error) {
	form := url.Values{"q": {query}}
	if df := map[string]string{"day": "d", "week": "w", "month": "m", "year": "y"}[opts.timeRange]; df != "" {
		form.Set("df", df)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ddgEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", firstNonEmpty(ua, defaultUserAgent))
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSearchBytes))
	if err != nil {
		return nil, err
	}
	return parseDuckDuckGo(string(body), limit), nil
}

// parseDuckDuckGo extracts results from the DuckDuckGo HTML endpoint. The
// markup uses .result__a for the title link and .result__snippet for the
// snippet; the href is a redirect carrying the real URL in the uddg= param.
func parseDuckDuckGo(doc string, limit int) []searchResult {
	root := parseHTML(doc)
	if root == nil {
		return nil
	}
	titles := selectNodes(root, ".result__a")
	snippets := selectNodes(root, ".result__snippet")
	out := make([]searchResult, 0, limit)
	for i, a := range titles {
		if len(out) >= limit {
			break
		}
		href := decodeDDGHref(attr(a, "href"))
		if href == "" {
			continue
		}
		r := searchResult{Title: strings.TrimSpace(textOf(a)), URL: href}
		if i < len(snippets) {
			r.Snippet = strings.TrimSpace(textOf(snippets[i]))
		}
		out = append(out, r)
	}
	return out
}

// decodeDDGHref resolves DuckDuckGo result hrefs to the real target URL.
// DDG uses direct URLs (https://target.com) in the current HTML format;
// older versions used redirect links with uddg= query param — both handled.
func decodeDDGHref(href string) string {
	if href == "" {
		return ""
	}
	if strings.HasPrefix(href, "//") {
		href = "https:" + href
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	if uddg := u.Query().Get("uddg"); uddg != "" {
		return uddg
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		// Skip DDG-internal navigation links (favicon, CSS, /html/, etc.)
		if strings.Contains(u.Host, "duckduckgo.com") {
			return ""
		}
		return u.String()
	}
	return ""
}

func searchSearxng(ctx context.Context, client *http.Client, cfg Config, query string, limit int, opts searchOpts) ([]searchResult, error) {
	base := firstNonEmpty(cfg.APIKeys["searxng_url"], searxngDefault)
	q := url.Values{"q": {query}, "format": {"json"}, "pageno": {"1"}}
	if opts.timeRange != "" {
		q.Set("time_range", opts.timeRange) // searxng accepts day/week/month/year
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/search?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := readJSON(resp, &payload); err != nil {
		return nil, err
	}
	return mapResults(len(payload.Results), limit, func(i int) searchResult {
		r := payload.Results[i]
		return searchResult{Title: r.Title, URL: r.URL, Snippet: r.Content}
	}), nil
}

func searchBrave(ctx context.Context, client *http.Client, cfg Config, query string, limit int, opts searchOpts) ([]searchResult, error) {
	key := cfg.APIKeys["brave"]
	if key == "" {
		return nil, fmt.Errorf("brave search requires api_keys.brave")
	}
	q := url.Values{"q": {query}, "count": {strconv.Itoa(limit)}}
	if f := map[string]string{"day": "pd", "week": "pw", "month": "pm", "year": "py"}[opts.timeRange]; f != "" {
		q.Set("freshness", f)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, braveEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Subscription-Token", key)
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := readJSON(resp, &payload); err != nil {
		return nil, err
	}
	return mapResults(len(payload.Web.Results), limit, func(i int) searchResult {
		r := payload.Web.Results[i]
		return searchResult{Title: r.Title, URL: r.URL, Snippet: r.Description}
	}), nil
}

func searchTavily(ctx context.Context, client *http.Client, cfg Config, query string, limit int, opts searchOpts) ([]searchResult, error) {
	key := cfg.APIKeys["tavily"]
	if key == "" {
		return nil, fmt.Errorf("tavily search requires api_keys.tavily")
	}
	reqBody := map[string]any{"api_key": key, "query": query, "max_results": limit}
	if opts.timeRange != "" {
		reqBody["time_range"] = opts.timeRange // tavily accepts day/week/month/year
	}
	bodyReq, _ := json.Marshal(reqBody)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tavilyEndpoint, strings.NewReader(string(bodyReq)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := readJSON(resp, &payload); err != nil {
		return nil, err
	}
	// Tavily already returns extracted content per result — populate Content
	// natively so the enrichment layer skips a redundant re-fetch.
	return mapResults(len(payload.Results), limit, func(i int) searchResult {
		r := payload.Results[i]
		return searchResult{Title: r.Title, URL: r.URL, Snippet: truncateRunes(r.Content, 300), Content: truncateRunes(r.Content, maxInlineContentChars)}
	}), nil
}

func searchGoogle(ctx context.Context, client *http.Client, cfg Config, query string, limit int, opts searchOpts) ([]searchResult, error) {
	key := cfg.APIKeys["google"]
	cx := cfg.APIKeys["google_cx"]
	if key == "" || cx == "" {
		return nil, fmt.Errorf("google search requires api_keys.google and api_keys.google_cx")
	}
	num := limit
	if num > 10 {
		num = 10 // Google CSE hard cap
	}
	q := url.Values{"key": {key}, "cx": {cx}, "q": {query}, "num": {strconv.Itoa(num)}}
	if d := map[string]string{"day": "d1", "week": "w1", "month": "m1", "year": "y1"}[opts.timeRange]; d != "" {
		q.Set("dateRestrict", d)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleEndpoint+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	var payload struct {
		Items []struct {
			Title   string `json:"title"`
			Link    string `json:"link"`
			Snippet string `json:"snippet"`
		} `json:"items"`
	}
	if err := readJSON(resp, &payload); err != nil {
		return nil, err
	}
	return mapResults(len(payload.Items), limit, func(i int) searchResult {
		r := payload.Items[i]
		return searchResult{Title: r.Title, URL: r.Link, Snippet: r.Snippet}
	}), nil
}

// mapResults builds up to limit results from a backend payload via get(i).
func mapResults(n, limit int, get func(int) searchResult) []searchResult {
	if n > limit {
		n = limit
	}
	out := make([]searchResult, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, get(i))
	}
	return out
}
