// Package safehttp provides SSRF-hardened HTTP transports shared by every
// component that fetches an LLM- or config-supplied URL (the web tool, the
// indexer's crawler, …). It is the single source of truth for the egress
// policy so a fix lands once, everywhere.
package safehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ErrBlockedHost is returned when a host resolves only to addresses the egress
// policy forbids (private, loopback, link-local, …).
var ErrBlockedHost = errors.New("blocked by SSRF guard")

// BlockedIP reports whether ip is one an untrusted fetch must never reach:
// loopback, RFC1918/ULA private, link-local (incl. 169.254.169.254 cloud
// metadata), multicast, unspecified, or CGNAT. IPv4-mapped IPv6 is unmapped
// first so ::ffff:127.0.0.1 is caught.
func BlockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() ||
		ip.IsUnspecified() ||
		cgnat(ip)
}

func cgnat(ip net.IP) bool {
	v4 := ip.To4()
	return v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127
}

// guardedDial resolves the host, rejects every blocked address, and dials a
// vetted IP directly — closing the DNS-rebinding window because the kernel
// connects to the address we validated, not a re-resolution.
func guardedDial(resolver *net.Resolver) func(ctx context.Context, network, addr string) (net.Conn, error) {
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
			if BlockedIP(ipa.IP) {
				lastErr = fmt.Errorf("%w: %s resolves to forbidden address %s", ErrBlockedHost, host, ipa.IP)
				continue
			}
			conn, derr := d.DialContext(ctx, network, net.JoinHostPort(ipa.IP.String(), port))
			if derr == nil {
				return conn, nil
			}
			lastErr = derr
		}
		if lastErr == nil {
			lastErr = fmt.Errorf("%w: %s did not resolve to any address", ErrBlockedHost, host)
		}
		return nil, lastErr
	}
}

// Transport returns an HTTP transport whose every connection is vetted by the
// egress policy. When allowPrivate is true the guard is dropped (operator
// opt-in for indexing internal hosts).
func Transport(allowPrivate bool) *http.Transport {
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
		tr.DialContext = guardedDial(net.DefaultResolver)
	}
	return tr
}

// Client builds an HTTP client whose every hop (including redirects) is vetted
// by Transport; checkRedirect re-applies any per-hop policy and timeout bounds
// the whole request.
func Client(timeout time.Duration, allowPrivate bool, checkRedirect func(*http.Request, []*http.Request) error) *http.Client {
	return &http.Client{Timeout: timeout, Transport: Transport(allowPrivate), CheckRedirect: checkRedirect}
}
