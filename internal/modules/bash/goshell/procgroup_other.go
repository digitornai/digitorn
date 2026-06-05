//go:build !windows

package goshell

import (
	"os/exec"
	"syscall"
)

// setProcGroup makes the child lead its own process group (pgid == pid), so a
// single signal to the negative pid reaps it plus anything it spawned.
func setProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcTree kills the child's whole process group, then the child itself —
// no orphans, no reliance on walking the parent/child tree.
func killProcTree(pid int) {
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
