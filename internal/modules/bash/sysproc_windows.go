//go:build windows

package bash

import (
	"os/exec"
	"strconv"
	"syscall"

	"github.com/mbathepaul/digitorn/internal/runtime/background"
)

var (
	errSignalINT  = background.ErrSignalINT
	errSignalTERM = background.ErrSignalTERM
)

// Windows doesn't have UNIX signals; use numeric constants as placeholders.
const (
	syscallSIGINT  = syscall.Signal(0x2)
	syscallSIGTERM = syscall.Signal(0xf)
)

func setSysProc(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessTree reaps the shell and its whole descendant tree. taskkill /T is
// the Windows-native way to terminate a process and all of its children.
func killProcessTree(pid int) {
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}

// signalProcessTree on Windows: no UNIX signal semantics, always kills forcefully.
func signalProcessTree(pid int, _ syscall.Signal) {
	killProcessTree(pid)
}
