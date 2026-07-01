package turn_test

import (
	"strings"
	"testing"

	"github.com/digitornai/digitorn/internal/runtime/turn"
)

func TestPhase_IsTerminal(t *testing.T) {
	cases := map[turn.Phase]bool{
		turn.PhasePending:     false,
		turn.PhaseLoading:     false,
		turn.PhaseRunning:     false,
		turn.PhasePersisting:  false,
		turn.PhaseDone:        true,
		turn.PhaseErrored:     true,
		turn.PhaseInterrupted: true,
	}
	for p, want := range cases {
		if got := p.IsTerminal(); got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", p, got, want)
		}
	}
}

func TestPhase_IsInFlight(t *testing.T) {
	cases := map[turn.Phase]bool{
		turn.PhasePending:     false,
		turn.PhaseLoading:     true,
		turn.PhaseRunning:     true,
		turn.PhasePersisting:  true,
		turn.PhaseDone:        false,
		turn.PhaseErrored:     false,
		turn.PhaseInterrupted: false,
	}
	for p, want := range cases {
		if got := p.IsInFlight(); got != want {
			t.Errorf("%q.IsInFlight() = %v, want %v", p, got, want)
		}
	}
}

func TestCanTransition_HappyPath(t *testing.T) {
	// The single canonical success path.
	chain := []turn.Phase{
		turn.PhasePending,
		turn.PhaseLoading,
		turn.PhaseRunning,
		turn.PhasePersisting,
		turn.PhaseDone,
	}
	for i := 0; i < len(chain)-1; i++ {
		if !turn.CanTransition(chain[i], chain[i+1]) {
			t.Errorf("happy path step %d : %q → %q rejected", i, chain[i], chain[i+1])
		}
	}
}

func TestCanTransition_EveryNonTerminalCanError(t *testing.T) {
	for _, p := range []turn.Phase{
		turn.PhasePending,
		turn.PhaseLoading,
		turn.PhaseRunning,
		turn.PhasePersisting,
	} {
		if !turn.CanTransition(p, turn.PhaseErrored) {
			t.Errorf("non-terminal %q must be able to → errored", p)
		}
		if !turn.CanTransition(p, turn.PhaseInterrupted) {
			t.Errorf("non-terminal %q must be able to → interrupted", p)
		}
	}
}

func TestCanTransition_TerminalIsSink(t *testing.T) {
	terminals := []turn.Phase{turn.PhaseDone, turn.PhaseErrored, turn.PhaseInterrupted}
	for _, term := range terminals {
		for _, p := range turn.AllPhases() {
			if turn.CanTransition(term, p) {
				t.Errorf("terminal %q must not transition to %q", term, p)
			}
		}
	}
}

func TestCanTransition_SkipsAreRejected(t *testing.T) {
	// You cannot skip phases — every non-error path goes through each
	// intermediate.
	skips := []struct{ from, to turn.Phase }{
		{turn.PhasePending, turn.PhaseRunning},
		{turn.PhasePending, turn.PhasePersisting},
		{turn.PhasePending, turn.PhaseDone},
		{turn.PhaseLoading, turn.PhasePersisting},
		{turn.PhaseLoading, turn.PhaseDone},
		{turn.PhaseRunning, turn.PhaseDone},
	}
	for _, s := range skips {
		if turn.CanTransition(s.from, s.to) {
			t.Errorf("forbidden skip %q → %q was allowed", s.from, s.to)
		}
	}
}

func TestCanTransition_BackwardsRejected(t *testing.T) {
	backwards := []struct{ from, to turn.Phase }{
		{turn.PhaseLoading, turn.PhasePending},
		{turn.PhaseRunning, turn.PhaseLoading},
		{turn.PhasePersisting, turn.PhaseRunning},
	}
	for _, b := range backwards {
		if turn.CanTransition(b.from, b.to) {
			t.Errorf("backwards %q → %q was allowed", b.from, b.to)
		}
	}
}

func TestCanTransition_UnknownPhasesRejected(t *testing.T) {
	garbage := turn.Phase("totally-made-up")
	for _, p := range turn.AllPhases() {
		if turn.CanTransition(garbage, p) {
			t.Errorf("transition from unknown %q to %q was allowed", garbage, p)
		}
		if turn.CanTransition(p, garbage) {
			t.Errorf("transition from %q to unknown %q was allowed", p, garbage)
		}
	}
}

func TestValidate_NoOpReturnsError(t *testing.T) {
	if err := turn.Validate(turn.PhaseRunning, turn.PhaseRunning); err == nil {
		t.Fatal("expected error for self-transition")
	}
}

func TestValidate_IllegalReturnsDescriptiveError(t *testing.T) {
	err := turn.Validate(turn.PhaseDone, turn.PhaseLoading)
	if err == nil {
		t.Fatal("expected error for illegal transition")
	}
	if !strings.Contains(err.Error(), "done") || !strings.Contains(err.Error(), "loading") {
		t.Errorf("error must mention both phases : %v", err)
	}
}

func TestValidate_LegalReturnsNil(t *testing.T) {
	if err := turn.Validate(turn.PhaseRunning, turn.PhasePersisting); err != nil {
		t.Errorf("legal transition rejected : %v", err)
	}
}

func TestAllPhases_CoversEveryConstant(t *testing.T) {
	got := turn.AllPhases()
	// 8 phases since SG-5 added PhaseWaitingApproval (pending,
	// loading, running, waiting_approval, persisting, done, errored,
	// interrupted).
	if len(got) != 8 {
		t.Fatalf("AllPhases returned %d, want 8", len(got))
	}
	// Spot-check : every phase appears in the matrix as a key (even if
	// terminal with empty allowed-set).
	for _, p := range got {
		// Should not panic. Indirectly checks the matrix covers all phases.
		_ = p.IsTerminal()
		_ = p.IsInFlight()
	}
}
