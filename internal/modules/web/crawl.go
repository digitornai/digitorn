package web

import (
	"context"
	"net/http"
	"time"

	"github.com/digitornai/digitorn/internal/domain/tool"
)

// crawlSpec parameterizes a bounded breadth-first crawl starting at fetch's url.
type crawlSpec struct {
	Depth      int   `json:"depth"`       // link-follow depth (1 = start page only)
	MaxPages   int   `json:"max_pages"`   // hard cap on pages fetched
	SameDomain *bool `json:"same_domain"` // stay on the start host (default true)
}

// Crawl bounds — keep a crawl fast, cheap, and safe by construction.
const (
	crawlDefaultDepth = 2
	crawlMaxDepth     = 4
	crawlDefaultPages = 10
	crawlMaxPages     = 50
	crawlPageContent  = 1500 // per-page content excerpt
	crawlMaxLinks     = 40   // links considered per page for the frontier
)

// crawlSite runs a bounded BFS from startURL, following same-domain links, and
// returns a compact model per visited page (url, title, content excerpt, link
// count). Every page fetch is SSRF- and domain-guarded like a normal fetch.
func (m *Module) crawlSite(ctx context.Context, sessionKey, startURL string, spec crawlSpec, cfg Config, client *http.Client, rt time.Duration) tool.Result {
	depth := clampInt(spec.Depth, 1, crawlMaxDepth, crawlDefaultDepth)
	maxPages := clampInt(spec.MaxPages, 1, crawlMaxPages, crawlDefaultPages)
	sameDomain := spec.SameDomain == nil || *spec.SameDomain
	startHost := hostOf(startURL)

	// crawl uses a dedicated tab so it never clobbers the session's interactive
	// page; the janitor reaps it when idle.
	crawlKey := sessionKey + "#crawl"

	type node struct {
		url   string
		depth int
	}
	queue := []node{{startURL, 1}}
	visited := map[string]bool{startURL: true}
	pages := make([]map[string]any, 0, maxPages)
	truncated := false

	for len(queue) > 0 {
		if len(pages) >= maxPages {
			truncated = true
			break
		}
		cur := queue[0]
		queue = queue[1:]

		if err := m.checkDomain(hostOf(cur.url)); err != nil {
			continue
		}
		html, finalURL, err := m.fetchPageHTML(ctx, crawlKey, cur.url, cfg, client, rt)
		if err != nil {
			continue
		}
		root := parseHTML(html)
		page := map[string]any{"url": finalURL, "depth": cur.depth}
		linkCount := 0
		if root != nil {
			if t := extractTitle(root); t != "" {
				page["title"] = t
			}
			page["content"] = truncateRunes(render(mainContent(root), false), crawlPageContent)
			pm := buildPageModel(root, finalURL)
			linkCount = len(pm.Links)
			// Expand the frontier from this page's links.
			if cur.depth < depth {
				for i, l := range pm.Links {
					if i >= crawlMaxLinks {
						break
					}
					if visited[l.URL] || len(visited) > maxPages*8 {
						continue
					}
					if sameDomain && hostOf(l.URL) != startHost {
						continue
					}
					visited[l.URL] = true
					queue = append(queue, node{l.URL, cur.depth + 1})
				}
			}
		}
		page["links"] = linkCount
		pages = append(pages, page)
	}

	// Free the crawl tab promptly (breadth crawls can be large).
	if m.browser != nil {
		m.browser.closeTab(ctx, crawlKey, false)
	}

	return tool.Result{
		Success: true,
		Data: map[string]any{
			"start":      startURL,
			"pages":      pages,
			"page_count": len(pages),
			"depth":      depth,
			"truncated":  truncated,
		},
		Display: &tool.DisplayHint{Type: "markdown", Title: "crawl " + startHost},
	}
}

// fetchPageHTML retrieves one page's HTML (HTTP first, headless render on a
// JS shell) for the crawler, returning the HTML and the final URL.
func (m *Module) fetchPageHTML(ctx context.Context, tabKey, urlStr string, cfg Config, client *http.Client, rt time.Duration) (string, string, error) {
	doc, err := m.download(ctx, client, urlStr)
	if err != nil {
		return "", "", err
	}
	if doc.binaryNote != "" {
		return "", doc.finalURL, nil // non-text: skip content, keep the url
	}
	html, finalURL := doc.body, doc.finalURL
	if m.browser != nil {
		visible := ""
		if probe := parseHTML(html); probe != nil {
			visible = render(probe, false)
		}
		if looksLikeJSShell(html, visible) {
			if rr, rerr := m.browser.navigate(ctx, tabKey, finalURL, cfg.AllowPrivateHosts, rt, m.checkDomain, false, false, false); rerr == nil && rr.html != "" {
				html = rr.html
				finalURL = firstNonEmpty(rr.finalURL, finalURL)
			}
		}
	}
	return html, finalURL, nil
}

func clampInt(v, lo, hi, def int) int {
	if v <= 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
