package safego_test

import (
	"strings"
	"testing"
	"time"

	"github.com/digitornai/digitorn/internal/safego"
)

type seenPanic struct {
	name string
	rec  any
}

// TestGo_PanicNeverEscapes proves the headline guarantee: a goroutine spawned
// via safego.Go that panics does NOT take the process down — the panic is
// recovered, and the rest of the program keeps running. The Reporter channel
// also synchronises the test: it fires only after the panic was recovered.
func TestGo_PanicNeverEscapes(t *testing.T) {
	got := make(chan seenPanic, 1)
	safego.Reporter = func(name string, rec any, _ []byte) { got <- seenPanic{name, rec} }
	t.Cleanup(func() { safego.Reporter = nil })

	safego.Go("boom", func() { panic("kaboom") })

	select {
	case s := <-got:
		if s.name != "boom" || s.rec != "kaboom" {
			t.Fatalf("recovered %+v, want {boom kaboom}", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("panic was not recovered within 2s (did it escape and crash a goroutine?)")
	}
}

// TestReport_ShapesAResult proves the pattern used by hot paths (engine tool
// goroutine, timeout): an inline recover() captures the panic, safego.Report
// logs it and returns a message the caller folds into an errored result.
func TestReport_ShapesAResult(t *testing.T) {
	got := func() (msg string) {
		defer func() {
			if r := recover(); r != nil {
				msg = safego.Report("unit", r)
			}
		}()
		panic("bad index")
	}()
	if !strings.Contains(got, "panic recovered") || !strings.Contains(got, "bad index") {
		t.Fatalf("Report message = %q", got)
	}
}
