package indexer

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	htmltomd "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/gocolly/colly/v2"

	"github.com/digitornai/digitorn/internal/safehttp"
)

func init() { Register(&webConnector{}) }

type webConnector struct{}

func (*webConnector) Type() string         { return "web" }
func (*webConnector) Capabilities() Caps    { return Caps{Walk: true} }
func (*webConnector) Watch(context.Context, SourceSpec, Sink, Cursor) error {
	return nil
}

type webOpts struct {
	URL          string
	MaxPages     int
	MaxDepth     int
	SameDomain   bool
	Sitemap      bool
	RespectRobot bool
	AllowPrivate bool
	RateLimit    time.Duration
	Parallelism  int
	Include      []*regexp.Regexp
	Exclude      []*regexp.Regexp
}

func (*webConnector) Walk(ctx context.Context, spec SourceSpec, emit func(Document) error) error {
	o := parseWebOpts(spec.Opts)
	seed, err := url.Parse(o.URL)
	if err != nil || (seed.Scheme != "http" && seed.Scheme != "https") {
		return errInvalidURL
	}

	c := colly.NewCollector(colly.Async(true))
	c.WithTransport(safehttp.Transport(o.AllowPrivate))
	if o.MaxDepth > 0 {
		c.MaxDepth = o.MaxDepth
	}
	c.IgnoreRobotsTxt = !o.RespectRobot
	if o.SameDomain {
		c.AllowedDomains = []string{seed.Hostname()}
	}
	if len(o.Include) > 0 {
		c.URLFilters = o.Include
	}
	if len(o.Exclude) > 0 {
		c.DisallowedURLFilters = o.Exclude
	}
	_ = c.Limit(&colly.LimitRule{DomainGlob: "*", Parallelism: o.Parallelism, Delay: o.RateLimit})

	var mu sync.Mutex
	pages := map[string]Document{}

	c.OnResponse(func(r *colly.Response) {
		if !strings.Contains(strings.ToLower(r.Headers.Get("Content-Type")), "html") {
			return
		}
		mu.Lock()
		over := len(pages) >= o.MaxPages
		mu.Unlock()
		if over {
			return
		}
		md, e := htmltomd.ConvertString(string(r.Body))
		if e != nil || strings.TrimSpace(md) == "" {
			return
		}
		u := canonical(r.Request.URL)
		doc := Document{ID: u, Text: strings.TrimSpace(md), Meta: map[string]any{"url": u}}
		if t := htmlTitle(string(r.Body)); t != "" {
			doc.Meta["title"] = t
		}
		mu.Lock()
		if len(pages) < o.MaxPages {
			pages[u] = doc
		}
		mu.Unlock()
	})

	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		mu.Lock()
		over := len(pages) >= o.MaxPages
		mu.Unlock()
		if over {
			return
		}
		_ = e.Request.Visit(e.Attr("href"))
	})

	if o.Sitemap {
		for _, u := range fetchSitemap(ctx, seed, o.AllowPrivate) {
			if o.SameDomain {
				if pu, err := url.Parse(u); err != nil || pu.Hostname() != seed.Hostname() {
					continue
				}
			}
			_ = c.Visit(u)
		}
	}
	_ = c.Visit(canonical(seed))
	c.Wait()

	for _, d := range pages {
		if err := emit(d); err != nil {
			return err
		}
	}
	return nil
}

func fetchSitemap(ctx context.Context, seed *url.URL, allowPrivate bool) []string {
	root := seed.Scheme + "://" + seed.Host + "/sitemap.xml"
	return sitemapURLs(ctx, root, 0, safehttp.Client(20*time.Second, allowPrivate, nil))
}

func sitemapURLs(ctx context.Context, smURL string, depth int, client *http.Client) []string {
	if depth > 2 {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, smURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "DigitornIndexer/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))

	var doc struct {
		URLs []struct {
			Loc string `xml:"loc"`
		} `xml:"url"`
		Sitemaps []struct {
			Loc string `xml:"loc"`
		} `xml:"sitemap"`
	}
	if xml.Unmarshal(body, &doc) != nil {
		return nil
	}
	var out []string
	for _, u := range doc.URLs {
		if s := strings.TrimSpace(u.Loc); s != "" {
			out = append(out, s)
		}
	}
	for _, sm := range doc.Sitemaps {
		out = append(out, sitemapURLs(ctx, strings.TrimSpace(sm.Loc), depth+1, client)...)
	}
	return out
}

func parseWebOpts(opts map[string]any) webOpts {
	o := webOpts{SameDomain: true, Sitemap: true, RespectRobot: true, MaxPages: 100, Parallelism: 4, RateLimit: 150 * time.Millisecond}
	o.URL = optString(opts, "url")
	if v, ok := optInt(opts, "max_pages"); ok {
		o.MaxPages = v
	}
	if v, ok := optInt(opts, "max_depth"); ok {
		o.MaxDepth = v
	}
	if v, ok := optInt(opts, "parallelism"); ok && v > 0 {
		o.Parallelism = v
	}
	if v, ok := optBool(opts, "same_domain"); ok {
		o.SameDomain = v
	}
	if v, ok := optBool(opts, "sitemap"); ok {
		o.Sitemap = v
	}
	if v, ok := optBool(opts, "respect_robots"); ok {
		o.RespectRobot = v
	}
	if v, ok := optBool(opts, "allow_private"); ok {
		o.AllowPrivate = v
	}
	if s := optString(opts, "rate_limit"); s != "" {
		if d, err := time.ParseDuration(s); err == nil {
			o.RateLimit = d
		}
	}
	o.Include = compileRegexes(optStrings(opts, "include"))
	o.Exclude = compileRegexes(optStrings(opts, "exclude"))
	return o
}

func compileRegexes(pats []string) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range pats {
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

func canonical(u *url.URL) string {
	c := *u
	c.Fragment = ""
	return c.String()
}

var htmlTitleRe = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
var htmlTagRe = regexp.MustCompile(`(?s)<[^>]+>`)

func htmlTitle(body string) string {
	if m := htmlTitleRe.FindStringSubmatch(body); m != nil {
		return strings.TrimSpace(htmlTagRe.ReplaceAllString(m[1], ""))
	}
	return ""
}
