//go:build linux

package sessionstore

import (
	"os"
	"syscall"
)

func fdatasyncFile(f *os.File) error {
	return syscall.Fdatasync(int(f.Fd()))
}
