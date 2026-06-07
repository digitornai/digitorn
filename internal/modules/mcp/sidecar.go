package mcp

import (
	"bytes"
	_ "embed"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

//go:embed sdk_fix_wrapper.py
var sdkFixWrapper []byte

var nonPythonCommands = map[string]bool{
	"node": true, "npx": true, "bun": true, "bunx": true,
	"deno": true, "docker": true, "podman": true,
}

var (
	wrapperOnce sync.Once
	wrapperPath string
)

func wrapPython(command string, args []string) (string, []string) {
	if strings.Contains(command, "sdk_fix_wrapper") {
		return command, args
	}
	for i, a := range args {
		if i >= 4 {
			break
		}
		if strings.Contains(a, "sdk_fix_wrapper") {
			return command, args
		}
	}
	base := filepath.Base(command)
	if nonPythonCommands[base] {
		return command, args
	}
	wrapper := ensureWrapper()
	if wrapper == "" {
		return command, args
	}
	if isPythonCommand(base) {
		return command, append([]string{wrapper}, args...)
	}
	if isPythonScript(command) {
		py := pythonExe()
		if py == "" {
			return command, args
		}
		return py, append([]string{wrapper, command}, args...)
	}
	return command, args
}

func isPythonCommand(base string) bool {
	return base == "python" || strings.HasPrefix(base, "python3")
}

func isPythonScript(command string) bool {
	path, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 512)
	n, _ := f.Read(head)
	head = head[:n]
	if bytes.HasPrefix(head, []byte("#!")) {
		first := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			first = head[:i]
		}
		if bytes.Contains(first, []byte("python")) {
			return true
		}
	}
	if bytes.Contains(head, []byte("import ")) || bytes.Contains(head, []byte("from ")) {
		if bytes.HasPrefix(head, []byte("#!/bin/sh")) || bytes.HasPrefix(head, []byte("#!/bin/bash")) {
			return false
		}
		firstLine := head
		if i := bytes.IndexByte(head, '\n'); i >= 0 {
			firstLine = head[:i]
		}
		if bytes.Contains(firstLine, []byte("node")) {
			return false
		}
		if bytes.Contains(head, []byte("import {")) || bytes.Contains(head, []byte(`from "`)) || bytes.Contains(head, []byte("from '")) {
			return false
		}
		return true
	}
	return false
}

func pythonExe() string {
	for _, name := range []string{"python3", "python"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func ensureWrapper() string {
	wrapperOnce.Do(func() {
		home, err := os.UserHomeDir()
		if err != nil {
			return
		}
		dir := filepath.Join(home, ".digitorn", "mcp")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
		path := filepath.Join(dir, "sdk_fix_wrapper.py")
		if err := os.WriteFile(path, sdkFixWrapper, 0o600); err != nil {
			return
		}
		wrapperPath = path
	})
	return wrapperPath
}
