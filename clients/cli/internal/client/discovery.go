package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const DefaultDaemonURL = "http://127.0.0.1:8000"

const EnvDaemonURL = "DIGITORN_URL"

func Discover() string {
	if u := strings.TrimSpace(os.Getenv(EnvDaemonURL)); u != "" {
		return u
	}
	return DefaultDaemonURL
}

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

type DaemonUnreachableError struct {
	URL   string
	Cause error
}

func (e *DaemonUnreachableError) Error() string {
	return fmt.Sprintf("daemon unreachable at %s : %v", e.URL, e.Cause)
}

func (e *DaemonUnreachableError) Unwrap() error { return e.Cause }

func IsUnreachable(err error) bool {
	var target *DaemonUnreachableError
	return errors.As(err, &target)
}
