//go:build !windows

package bash

import (
	"os/exec"
	"syscall"
)

func setSysProc(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessTree kills the shell and every process in its group. Because the
// shell was started with Setpgid it leads its own group (pgid == pid), so a
// single signal to -pid reaps the shell plus any command it spawned — no
// orphans, no reliance on walking the parent/child tree.
func killProcessTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
