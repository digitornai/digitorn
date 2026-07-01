// Package version exposes build identity, injected at link time via
//
//	-ldflags "-X github.com/digitornai/digitorn/internal/version.Version=v1.2.3 \
//	          -X github.com/digitornai/digitorn/internal/version.Commit=abc1234 \
//	          -X github.com/digitornai/digitorn/internal/version.Date=2026-05-31"
//
// Unset, it reports a dev build.
package version

import (
	"fmt"
	"runtime"
)

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// String renders a single human-readable build line.
func String() string {
	return fmt.Sprintf("%s (commit %s, built %s, %s)", Version, Commit, Date, runtime.Version())
}
