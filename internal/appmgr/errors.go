// Package appmgr installs, lists, and serves digitorn apps. The disk
// layout is dead simple — one directory per app under config.Apps.Root,
// containing the source files copied verbatim PLUS an app.dgc
// compiled bundle produced by the compiler. The runtime loads app.dgc
// directly (JVM-style) ; the YAML source stays on disk only for
// human editing and reload.
package appmgr

import "errors"

// ErrAppNotFound is returned when an app_id has no row in the DB.
var ErrAppNotFound = errors.New("appmgr: app not found")

// ErrAppDisabled is returned by Get when the app exists but its
// Enabled flag is false.
var ErrAppDisabled = errors.New("appmgr: app disabled")

// ErrBadSource is returned when an install source URI cannot be
// parsed or refers to a non-existent location.
var ErrBadSource = errors.New("appmgr: bad source")

// ErrSourceMissingYAML is returned when the source directory does not
// contain an app.yaml at its root.
var ErrSourceMissingYAML = errors.New("appmgr: source has no app.yaml")

// ErrAppIDMismatch is returned when the source dir's basename does
// not match the app_id declared in app.yaml. The strict-folder-name
// rule keeps every lookup O(1) — no extra index table needed.
var ErrAppIDMismatch = errors.New("appmgr: source dir basename must equal app_id")

// ErrCompileFailed is returned when the compiler reports errors on
// the source. The wrapped error contains the diagnostic codes.
var ErrCompileFailed = errors.New("appmgr: compile failed")

// ErrHubFetch wraps any hub-side fetch failure.
var ErrHubFetch = errors.New("appmgr: hub fetch failed")

// ErrArchiveTooBig is returned when the downloaded hub archive
// exceeds config.Apps.Hub.MaxArchiveBytes.
var ErrArchiveTooBig = errors.New("appmgr: hub archive exceeds size limit")

// ErrArchiveTraversal is returned when a tar member tries to escape
// the install dir (absolute path, .. component, symlink, hardlink).
var ErrArchiveTraversal = errors.New("appmgr: hub archive contains unsafe path")
