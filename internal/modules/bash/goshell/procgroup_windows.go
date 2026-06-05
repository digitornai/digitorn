package goshell

import (
	"os/exec"
	"strconv"
	"syscall"
)

// setProcGroup starts the child in its own process group so a cancel can reap it
// and everything it spawns without touching the daemon itself.
func setProcGroup(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcTree terminates a process and its whole descendant tree. taskkill /T
// follows the kernel's parent/child records, so it catches grandchildren (a dev
// server's node → workers) that killing the direct child alone would orphan.
func killProcTree(pid int) {
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
