//go:build !windows

package bash

// enrichPath is a no-op off Windows : a normally-launched Unix daemon inherits a
// complete PATH, and there is no per-user registry PATH to merge.
func enrichPath(current string) string { return current }
