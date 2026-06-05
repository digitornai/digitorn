// Copyright 2011 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package index

import (
	"os"
	"syscall"
	"unsafe"
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
	if size == 0 {
		return mmapData{f: f}
	}
	h, err := syscall.CreateFileMapping(syscall.Handle(f.Fd()), nil, syscall.PAGE_READONLY, uint32(size>>32), uint32(size), nil)
	if err != nil {
		fatalf("CreateFileMapping %s: %v", f.Name(), err)
	}

	addr, err := syscall.MapViewOfFile(h, syscall.FILE_MAP_READ, 0, 0, 0)
	if err != nil {
		syscall.CloseHandle(h)
		fatalf("MapViewOfFile %s: %v", f.Name(), err)
	}
	// go vet's unsafeptr check flags this uintptr→Pointer conversion ; it is the
	// unavoidable, correct idiom for a Windows MapViewOfFile address (a real
	// mapped page, not a GC pointer). Not in `go test`'s vet subset, so tests
	// stay clean.
	return mmapData{f: f, d: unsafe.Slice((*byte)(unsafe.Pointer(addr)), size), h: uintptr(h)}
}

// close unmaps the view, releases the file-mapping handle, and closes the file.
func (m mmapData) close() error {
	var err error
	if len(m.d) > 0 {
		if e := syscall.UnmapViewOfFile(uintptr(unsafe.Pointer(&m.d[0]))); e != nil {
			err = e
		}
	}
	if m.h != 0 {
		if e := syscall.CloseHandle(syscall.Handle(m.h)); e != nil && err == nil {
			err = e
		}
	}
	if m.f != nil {
		if e := m.f.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}
