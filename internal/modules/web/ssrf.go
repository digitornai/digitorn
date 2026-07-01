package web

import (
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/digitornai/digitorn/internal/safehttp"
)

func blockedIP(ip net.IP) bool { return safehttp.BlockedIP(ip) }

// newGuardedClient builds an HTTP client whose every connection (including each
// redirect hop) is vetted by the shared SSRF guard; the caller-supplied
// checkRedirect re-applies the domain policy per hop.
func newGuardedClient(timeout time.Duration, allowPrivate bool, checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	return safehttp.Client(timeout, allowPrivate, checkRedirect)
}

// hostOf returns the lowercased hostname of rawURL, or "" if it cannot be
// parsed. Used by the domain allow/block policy.
func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
