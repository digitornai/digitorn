// Package safego runs goroutines that can never take the process down. Every
// shielded function is wrapped in a recover that logs the panic with its stack
// and forwards it to an optional reporter, so a bug in one goroutine degrades
// to a logged incident instead of crashing the whole daemon.
//
// The rule for the daemon: NO bare `go func()` in long-lived code. Spawn with
// Go, or guard an existing goroutine with `defer safego.Recover(name)`.
package safego

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Reporter, when set, is called for every recovered panic (metrics, alerting).
// It must not panic; if it does, the panic is swallowed. Set once at boot.
var Reporter func(name string, recovered any, stack []byte)

// Go spawns fn in a new goroutine under a panic shield. name identifies the
// goroutine in logs. A panic in fn is recovered, logged and reported — it never
// propagates to the runtime, so it cannot crash the process.
func Go(name string, fn func()) {
	go Run(name, fn)
}

// Run executes fn under the panic shield in the CURRENT goroutine — for callers
// that already own one (worker loops, dispatch goroutines started elsewhere).
func Run(name string, fn func()) {
	defer Recover(name)
	fn()
}

// Recover is the deferred guard. Register it as the FIRST defer of a goroutine
// (so it runs last, after WaitGroup.Done and friends):
//
//	go func() {
//	    defer safego.Recover("my-loop")
//	    defer wg.Done()
//	    ...
//	}()
func Recover(name string) {
	if r := recover(); r != nil {
		report(name, r)
	}
}

// RecoverErr is the deferred guard for code that must surface the panic as a
// returned error rather than only logging it. Pass the address of the function's
// named error return; it is set only when the function did not already fail.
//
//	func do() (err error) {
//	    defer safego.RecoverErr("do", &err)
//	    ...
//	}
func RecoverErr(name string, errp *error) {
	if r := recover(); r != nil {
		report(name, r)
		if errp != nil && *errp == nil {
			*errp = fmt.Errorf("panic in %s (recovered): %v", name, r)
		}
	}
}

// Report logs and forwards an ALREADY-recovered panic value r and returns a
// human-readable message for an errored result. recover() only works when
// called directly by a deferred function, so the caller must capture the panic
// inline and hand the value here:
//
//	defer func() {
//	    if r := recover(); r != nil {
//	        outcomes[i] = errored(safego.Report("engine.tool", r))
//	    }
//	}()
func Report(name string, r any) string {
	report(name, r)
	return fmt.Sprintf("internal error (panic recovered): %v", r)
}

func report(name string, r any) {
	stack := debug.Stack()
	slog.Error("safego: goroutine panicked (recovered)",
		slog.String("goroutine", name),
		slog.Any("panic", r),
		slog.String("stack", string(stack)),
	)
	if Reporter != nil {
		func() {
			defer func() { _ = recover() }() // a faulty reporter must not crash us
			Reporter(name, r, stack)
		}()
	}
}
