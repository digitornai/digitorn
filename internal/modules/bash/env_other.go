//go:build !windows

package bash

func enrichPath(current string) string { return current }
