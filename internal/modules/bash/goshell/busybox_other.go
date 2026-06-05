//go:build !windows

package goshell

// busyboxDir is a no-op on non-Windows hosts: Linux and macOS already ship bash
// and the GNU/BSD coreutils, so nothing is embedded and the daemon stays lean.
func busyboxDir() string { return "" }
