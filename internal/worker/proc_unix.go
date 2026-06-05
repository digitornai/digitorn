//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package worker

import (
	"os"
	"os/exec"
	"syscall"
)

// configureProcAttr puts each worker in its own process group so we can
// signal the whole group cleanly (e.g. on shutdown) without touching the
// parent process.
func configureProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// trackChild is a no-op on unix : workers already lead their own process group
// (Setpgid), so graceful shutdown reaps the whole group via a single signal.
// The Windows build binds children to a kill-on-close Job Object instead, since
// it has no process groups. (A hard daemon crash on Linux could still orphan the
// group — PR_SET_PDEATHSIG would close that last gap.)
func trackChild(cmd *exec.Cmd) {}

// sendStopSignal sends SIGTERM to the worker's whole process group. Because
// the child leads its own group (Setpgid), the negative pid reaches any
// grandchildren it spawned, so nothing is orphaned.
func sendStopSignal(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGTERM); err != nil {
		return p.Signal(syscall.SIGTERM)
	}
	return nil
}

// hardKill SIGKILLs the whole process group when the graceful stop times out.
func hardKill(p *os.Process) error {
	if p == nil {
		return nil
	}
	if err := syscall.Kill(-p.Pid, syscall.SIGKILL); err != nil {
		return p.Kill()
	}
	return nil
}
