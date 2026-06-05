package web

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// errBlockedHost is returned when a host resolves only to addresses the egress
// policy forbids (private, loopback, link-local, …). It is a sentinel so the
// dial layer and the redirect check report the same failure.
var errBlockedHost = errors.New("blocked by SSRF guard")

// blockedIP reports whether ip is one an LLM-driven fetch must never reach:
// loopback, RFC1918/ULA private, link-local (incl. 169.254.169.254 cloud
// metadata), multicast, unspecified, or CGNAT. IPv4-mapped IPv6 is unmapped
// first so ::ffff:127.0.0.1 is caught.
func blockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		isCGNAT(ip)
}

// isCGNAT reports whether ip is in the carrier-grade NAT range 100.64.0.0/10,
// which net.IP.IsPrivate does not cover.
func isCGNAT(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// safeDialContext returns a DialContext that resolves the target host, rejects
// every blocked address, and dials a vetted IP directly. Dialing the resolved
// IP (rather than the hostname) closes the DNS-rebinding window: the address
// the kernel connects to is the one we validated, not a re-resolution. TLS
// still verifies against the URL host because net/http derives the SNI/cert
// name from the request, not the dial address.
func safeDialContext(resolver *net.Resolver) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := resolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, err
		}
		var lastErr error
		for _, ipa := range ips {
			if blockedIP(ipa.IP) {
				lastErr = fmt.Errorf("%w: %s resolves to forbidden address %s", errBlockedHost, host, ipa.IP)
				continue
			}
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: %s did not resolve to any address", errBlockedHost, host)
		}
		return nil, lastErr
	}
}

// newGuardedClient builds an HTTP client whose every connection (including each
// redirect hop) is vetted by safeDialContext, redirects are capped, and the
// caller-supplied checkRedirect re-applies the domain policy per hop. timeout
// bounds the whole request. When allowPrivate is true the SSRF dial guard is
// dropped (operator opt-in for fetching internal hosts).
func newGuardedClient(timeout time.Duration, allowPrivate bool, checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if allowPrivate {
		tr.DialContext = (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	} else {
		tr.DialContext = safeDialContext(net.DefaultResolver)
	}
	return &http.Client{
		Timeout:       timeout,
		Transport:     tr,
		CheckRedirect: checkRedirect,
	}
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
