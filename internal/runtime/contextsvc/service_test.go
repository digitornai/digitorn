package contextsvc

import (
	"testing"

	"github.com/digitornai/digitorn/internal/compiler/schema"
	"github.com/digitornai/digitorn/internal/runtime/sessionstore"
)

func TestResolve_BudgetPressureAndRemaining(t *testing.T) {
	snap := sessionstore.SessionSnapshot{ContextTokens: 6000}
	brain := schema.Brain{
		Provider: "openai", Model: "gpt-4o",
		Context: &schema.ContextConfig{MaxTokens: 10000, OutputReserved: 2000},
	}
	cs := Resolve(snap, brain)

	if cs.Window != 10000 {
		t.Errorf("window = %d, want 10000", cs.Window)
	}
	if cs.MaxTokens != 8000 { // 10000 - 2000 reserve
		t.Errorf("max (usable input) = %d, want 8000", cs.MaxTokens)
	}
	if cs.TokensUsed != 6000 || !cs.HasAnchor {
		t.Errorf("occupancy = %d hasAnchor=%v, want 6000/true", cs.TokensUsed, cs.HasAnchor)
	}
	if cs.Pressure != 0.75 { // 6000/8000
		t.Errorf("pressure = %v, want 0.75", cs.Pressure)
	}
	if cs.Remaining != 2000 {
		t.Errorf("remaining = %d, want 2000", cs.Remaining)
	}
}

func TestResolve_AutoDetectWindowAndDefaultReserve(t *testing.T) {
	// max_tokens:0 → auto-detect from the provider/model table ; no
	// output_reserved → DefaultOutputReserved.
	cs := Resolve(sessionstore.SessionSnapshot{ContextTokens: 1000},
		schema.Brain{Provider: "anthropic", Model: "claude-3-5-sonnet"})
	if cs.Window <= 0 {
		t.Fatalf("window must auto-detect for a known model, got %d", cs.Window)
	}
	if cs.OutputReserved != DefaultOutputReserved {
		t.Errorf("reserve = %d, want default %d", cs.OutputReserved, DefaultOutputReserved)
	}
	if cs.MaxTokens != cs.Window-DefaultOutputReserved {
		t.Errorf("max = %d, want window-reserve = %d", cs.MaxTokens, cs.Window-DefaultOutputReserved)
	}
}

func TestResolve_NoAnchorIsZeroNotEmpty(t *testing.T) {
	cs := Resolve(sessionstore.SessionSnapshot{}, schema.Brain{Provider: "openai", Model: "gpt-4o"})
	if cs.HasAnchor {
		t.Error("no usage yet → HasAnchor must be false")
	}
	if cs.TokensUsed != 0 || cs.Pressure != 0 {
		t.Errorf("no anchor → used/pressure must be 0, got %d/%v", cs.TokensUsed, cs.Pressure)
	}
}

func TestResolve_CarriesCompactionView(t *testing.T) {
	snap := sessionstore.SessionSnapshot{
		ContextTokens:      500,
		CompactionInflight: true,
		ContextCompaction:  &sessionstore.ContextCompactionState{CutoffSeq: 42, Strategy: "summarize"},
	}
	cs := Resolve(snap, schema.Brain{Provider: "openai", Model: "gpt-4o"})
	if cs.CutoffSeq != 42 || cs.Strategy != "summarize" || !cs.CompactionInflight {
		t.Errorf("compaction view not carried: %+v", cs)
	}
}

// TestResolve_IsolationBetweenSessions : Resolve is pure, so two sessions'
// snapshots produce fully independent results — one session's occupancy can
// never bleed into another's pressure.
func TestResolve_IsolationBetweenSessions(t *testing.T) {
	brain := schema.Brain{Provider: "openai", Model: "gpt-4o", Context: &schema.ContextConfig{MaxTokens: 10000, OutputReserved: 0}}
	a := Resolve(sessionstore.SessionSnapshot{SessionID: "A", ContextTokens: 9000}, brain)
	b := Resolve(sessionstore.SessionSnapshot{SessionID: "B", ContextTokens: 10}, brain)
	if a.TokensUsed == b.TokensUsed {
		t.Fatal("sessions must not share occupancy")
	}
	if !(a.Pressure > 0.9) || !(b.Pressure < 0.01) {
		t.Errorf("isolation broken: A.pressure=%v B.pressure=%v", a.Pressure, b.Pressure)
	}
}

// BenchmarkResolve_O1VsSessionSize is the B2 acceptance bench : Resolve must
// cost the same whether the session holds 10 messages or 100 000 — proving it
// reads the gauge in O(1) and never scans the history. If these two diverge,
// a hidden O(n) scan crept onto the hot path.
func BenchmarkResolve_O1VsSessionSize(b *testing.B) {
	brain := schema.Brain{Provider: "openai", Model: "gpt-4o", Context: &schema.ContextConfig{MaxTokens: 128000}}
	mk := func(n int) sessionstore.SessionSnapshot {
		msgs := make([]sessionstore.Message, n)
		for i := range msgs {
			msgs[i] = sessionstore.Message{Role: "user", Content: "some message body text here"}
		}
		return sessionstore.SessionSnapshot{ContextTokens: 50000, Messages: msgs}
	}
	small, big := mk(10), mk(100000)

	b.Run("10msgs", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = Resolve(small, brain)
		}
	})
	b.Run("100000msgs", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			_ = Resolve(big, brain)
		}
	})
}
