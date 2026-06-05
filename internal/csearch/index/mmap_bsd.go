// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build darwin || freebsd || openbsd || netbsd
// +build darwin freebsd openbsd netbsd

package index

import (
	"os"
	"syscall"
)

// missing from package syscall on freebsd, openbsd
const (
	_PROT_READ  = 1
	_MAP_SHARED = 1
)

func mmapFile(f *os.File) mmapData {
	st, err := f.Stat()
	if err != nil {
		fatal(err)
	}
	size := st.Size()
	if int64(int(size+4095)) != size+4095 {
		fatalf("%s: too large for mmap", f.Name())
	}
	n := int(size)
	if n == 0 {
		return mmapData{f: f}
	}
	data, err := syscall.Mmap(int(f.Fd()), 0, (n+4095)&^4095, _PROT_READ, _MAP_SHARED)
	if err != nil {
		fatalf("mmap %s: %v", f.Name(), err)
	}
	return mmapData{f: f, d: data[:n]}
}

// close unmaps the page-rounded region (recovered via cap) and closes the file.
func (m mmapData) close() error {
	var err error
	if m.d != nil {
		err = syscall.Munmap(m.d[:cap(m.d)])
	}
	if m.f != nil {
		if e := m.f.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
