//go:build windows

package worker

import (
	"os"
	"os/exec"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows : no process groups in the POSIX sense, no SIGTERM. We rely on
// graceful gRPC shutdown via a Stop RPC, with Process.Kill as fallback — AND on
// a kill-on-close Job Object (see trackChild) so a worker can never outlive the
// daemon even on a hard crash.
func configureProcAttr(cmd *exec.Cmd) {
	// No-op : we intentionally do NOT use CREATE_NEW_PROCESS_GROUP because
	// it interferes with Ctrl+C handling on the parent. The worker should
	// listen on its gRPC Stop endpoint instead. Lifetime binding is done
	// post-start via the Job Object in trackChild.
}

var (
	jobOnce sync.Once
	jobH    windows.Handle
	jobErr  error
)

// killOnCloseJob lazily creates a process-wide Job Object whose limit flag
// KILL_ON_JOB_CLOSE makes the OS terminate every assigned process the instant
// the job's last handle closes. The daemon holds that handle for its entire
// life, so when it exits — gracefully, by panic, by `taskkill /F`, or because
// the machine OOM-killed it — the handle closes and Windows reaps every worker.
// This is what makes orphaned digitorn-worker-* processes structurally
// impossible, rather than something we hope a cleanup path remembers to do.
func killOnCloseJob() (windows.Handle, error) {
	jobOnce.Do(func() {
		h, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			jobErr = err
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
			BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
				LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
			},
		}
		if _, err := windows.SetInformationJobObject(
			h,
			windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)),
			uint32(unsafe.Sizeof(info)),
		); err != nil {
			_ = windows.CloseHandle(h)
			jobErr = err
			return
		}
		jobH = h // never closed : the OS closes it on daemon exit, which is the trigger.
	})
	return jobH, jobErr
}

// trackChild binds a freshly-started worker to the kill-on-close job so it dies
// with the daemon. Best-effort : any failure leaves the worker running (we only
// lose the auto-reap guarantee for it), so a Job API quirk never blocks startup.
// Child processes the worker itself spawns inherit the job, so grandchildren are
// covered too.
func trackChild(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := killOnCloseJob()
	if err != nil || job == 0 {
		return
	}
	h, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false, uint32(cmd.Process.Pid),
	)
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	_ = windows.AssignProcessToJobObject(job, h)
}

// sendStopSignal on Windows kills the worker. The framework's preferred
// shutdown path is the gRPC Stop RPC ; this is the fallback when the
// child is unresponsive.
func sendStopSignal(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}

// hardKill is the timeout fallback. On Windows Process.Kill already
// terminates the child; descendants are best-effort (no process groups),
// but the kill-on-close job covers the daemon-death case.
func hardKill(p *os.Process) error {
	if p == nil {
		return nil
	}
	return p.Kill()
}
