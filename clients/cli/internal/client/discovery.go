package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// DefaultDaemonURL is the loopback URL used when nothing else is
// configured — the canonical local dev port. Production deployments
// override via the DIGITORN_URL env var or ~/.digitorn/cli.toml.
const DefaultDaemonURL = "http://127.0.0.1:8000"

// EnvDaemonURL is the env var the CLI honors first. Set this to
// override any config file. Matches the convention DIGITORN_* used
// by the daemon's koanf config loader (so users only learn one
// prefix).
const EnvDaemonURL = "DIGITORN_URL"

// Discover returns the daemon URL the CLI should talk to, following
// a fixed precedence :
//
//  1. DIGITORN_URL env var (highest priority — easy local override)
//  2. The url passed via --daemon-url flag (handled by caller before
//     calling Discover ; if non-empty, caller skips Discover)
//  3. ~/.digitorn/cli.toml [daemon] url (CLI-2 lands the loader)
//  4. DefaultDaemonURL
//
// Discover does NOT attempt to probe / health-check the URL — that's
// the responsibility of the connection layer. Returning a string
// here is intentionally cheap : Ping() is the right tool for
// "is the daemon up ?".
func Discover() string {
	if u := strings.TrimSpace(os.Getenv(EnvDaemonURL)); u != "" {
		return u
	}
	// (CLI-2 will plug the cli.toml loader here.)
	return DefaultDaemonURL
}

// DiscoverAndPing combines Discover with a health probe. Useful for
// `digitorn list` which wants to fail fast with a clear message if
// the daemon isn't up.
func DiscoverAndPing(ctx context.Context, timeout time.Duration) (string, error) {
	url := Discover()
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	c, err := New(Options{BaseURL: url})
	if err != nil {
		return url, fmt.Errorf("discover: %w", err)
	}
	if err := c.Ping(probeCtx); err != nil {
		return url, &DaemonUnreachableError{URL: url, Cause: err}
	}
	return url, nil
}

// DaemonUnreachableError is returned when the daemon URL is set but
// /health doesn't respond. UI layers should render a clear "is the
// daemon running ? (try `digitornd -config ...`)" message.
type DaemonUnreachableError struct {
	URL   string
	Cause error
}

func (e *DaemonUnreachableError) Error() string {
	return fmt.Sprintf("daemon unreachable at %s : %v", e.URL, e.Cause)
}

func (e *DaemonUnreachableError) Unwrap() error { return e.Cause }

// IsUnreachable reports whether err is a DaemonUnreachableError.
// Use this in commands to print a friendly help blurb instead of the
// raw network error.
func IsUnreachable(err error) bool {
	var target *DaemonUnreachableError
	return errors.As(err, &target)
}
