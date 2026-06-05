package client

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// debugLog writes diagnostic lines to %TEMP%/digitorn-cli-debug.log. Enabled
// when DIGITORN_DEBUG=1. Cheap : disabled = noop.
//
// The TUI runs in alt-screen so stderr is invisible ; this file is the only
// way to see realtime / Socket.IO traces while the UI is up.
var (
	debugMu      sync.Mutex
	debugFile    *os.File
	debugInit    sync.Once
	debugEnabled bool
)

func initDebug() {
	debugEnabled = os.Getenv("DIGITORN_DEBUG") == "1"
	if !debugEnabled {
		return
	}
	tmp := os.TempDir()
	path := filepath.Join(tmp, "digitorn-cli-debug.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		debugEnabled = false
		return
	}
	debugFile = f
	fmt.Fprintf(f, "\n=== digitorn-cli started at %s ===\n", time.Now().Format(time.RFC3339))
}

// Debugf writes a single line to the debug log. No-op unless DIGITORN_DEBUG=1.
func Debugf(format string, args ...any) {
	debugInit.Do(initDebug)
	if !debugEnabled {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	if debugFile == nil {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(debugFile, "[%s] ", ts)
	fmt.Fprintf(debugFile, format, args...)
	if len(format) == 0 || format[len(format)-1] != '\n' {
		fmt.Fprint(debugFile, "\n")
	}
}
