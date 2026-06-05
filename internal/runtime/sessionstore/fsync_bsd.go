//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package sessionstore

import "os"

func fdatasyncFile(f *os.File) error {
	return f.Sync()
}
