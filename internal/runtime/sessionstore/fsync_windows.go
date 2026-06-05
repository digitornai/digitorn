//go:build windows

package sessionstore

import "os"

func fdatasyncFile(f *os.File) error {
	return f.Sync()
}
