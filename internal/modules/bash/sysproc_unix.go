//go:build !windows

package bash

import (
	"os/exec"
	"syscall"

	"github.com/digitornai/digitorn/internal/runtime/background"
)

// errSignalINT / errSignalTERM are the exact sentinel values from the background
// manager, so context.Cause() == errSignalINT works correctly (same pointer).
var (
	errSignalINT  = background.ErrSignalINT
	errSignalTERM = background.ErrSignalTERM
)

const (
	syscallSIGINT  = syscall.SIGINT
	syscallSIGTERM = syscall.SIGTERM
)

func setSysProc(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the shell and every process in its group with SIGKILL.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// signalProcessTree sends sig to the shell's process group, then the shell
// itself, allowing graceful shutdown (SIGINT → trap, SIGTERM → cleanup).
func signalProcessTree(pid int, sig syscall.Signal) {
	_ = syscall.Kill(-pid, sig) // process group (shell + all children)
	_ = syscall.Kill(pid, sig)  // shell process itself
}
