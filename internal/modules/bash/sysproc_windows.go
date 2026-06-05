//go:build windows

package bash

import (
	"os/exec"
	"strconv"
	"syscall"
)

func setSysProc(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// killProcessTree reaps the shell and its whole descendant tree. taskkill /T is
// the Windows-native way to terminate a process and all of its children — it
// follows the kernel's parent/child records and so catches msys2/Git-Bash
// children that a userspace process walk can miss.
func killProcessTree(pid int) {
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(pid)).Run()
}
