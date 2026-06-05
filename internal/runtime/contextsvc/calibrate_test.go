package contextsvc

import (
	"math"
	"testing"
)

// CalibrateTotal is THE guarantee: the displayed context total is the provider's
// EXACT count at every turn boundary, and a provider-calibrated estimate the
// rest of the time — for ANY provider, learned from that provider's own usage,
// nothing hard-coded.

func TestCalibrate_TurnEnd_ShowsExactProvider_AndLearnsRatio(t *testing.T) {
	// Tokenizer (tiktoken) says 1000, the provider really counted 1200 for the
	// SAME content (a turn just ended → fresh anchor). The displayed total MUST be
	// the provider's exact 1200, and the learned ratio MUST be 1200/1000 = 1.2.
	total, ratio := CalibrateTotal(1000, 1200, 0)
	if total != 1200 {
		t.Fatalf("turn-end total = %d, want 1200 (provider exact)", total)
	}
	if math.Abs(ratio-1.2) > 1e-9 {
		t.Fatalf("learned ratio = %.4f, want 1.2", ratio)
	}
}

func TestCalibrate_BetweenTurns_AppliesLearnedRatio(t *testing.T) {
	// No fresh anchor (between turns / right after a compaction the provider
	// hasn't seen). Apply the learned ratio to the raw tokenizer count.
	// 500 tiktoken × 1.2 = 600 provider-calibrated.
	total, _ := CalibrateTotal(500, 0, 1.2)
	if total != 600 {
		t.Fatalf("calibrated total = %d, want 600 (500 × 1.2)", total)
	}
}

func TestCalibrate_NotYetLearned_RawPassthrough(t *testing.T) {
	// No anchor and no learned ratio yet (first recount before any turn). Show the
	// raw tokenizer count unchanged — best available, never invented.
	total, ratio := CalibrateTotal(800, 0, 0)
	if total != 800 {
		t.Fatalf("total = %d, want 800 (raw passthrough)", total)
	}
	if ratio != 1.0 {
		t.Fatalf("ratio = %.4f, want 1.0", ratio)
	}
}

func TestCalibrate_EMASmoothing(t *testing.T) {
	// First turn learns 1.2 ; a second turn whose true ratio is 1.0 must be
	// absorbed smoothly (EMA), not jump: 0.5×1.2 + 0.5×1.0 = 1.1.
	_, r1 := CalibrateTotal(1000, 1200, 0)
	_, r2 := CalibrateTotal(1000, 1000, r1)
	if math.Abs(r2-1.1) > 1e-9 {
		t.Fatalf("EMA ratio = %.4f, want 1.1", r2)
	}
}

func TestCalibrate_ClampsExtremeRatio(t *testing.T) {
	// A pathological anchor/raw must not blow up the gauge: ratio clamped to
	// [0.25, 4.0]. raw=100, anchor=100000 → inst 1000 → clamped 4.0. But the
	// turn-end display is still the provider's exact count (the anchor).
	total, ratio := CalibrateTotal(100, 100000, 0)
	if total != 100000 {
		t.Fatalf("turn-end total = %d, want 100000 (provider exact)", total)
	}
	if ratio != 4.0 {
		t.Fatalf("ratio = %.4f, want clamped 4.0", ratio)
	}
	// And on the low side.
	_, rlo := CalibrateTotal(100000, 100, 0)
	if rlo != 0.25 {
		t.Fatalf("ratio = %.4f, want clamped 0.25", rlo)
	}
}

func TestCalibrate_ZeroRaw(t *testing.T) {
	// A zero raw count (nothing measured) yields zero, never a divide blow-up.
	total, _ := CalibrateTotal(0, 0, 1.2)
	if total != 0 {
		t.Fatalf("total = %d, want 0", total)
	}
}

// End-to-end of the guarantee across a session lifecycle: learn at turn 1,
// stay exact at turn boundaries, calibrate post-compaction, re-anchor at turn 2.
func TestCalibrate_SessionLifecycle(t *testing.T) {
	ratio := 0.0
	// Turn 1 ends: tiktoken 4000, provider 5000 → display 5000, learn 1.25.
	total, ratio := CalibrateTotal(4000, 5000, ratio)
	if total != 5000 || math.Abs(ratio-1.25) > 1e-9 {
		t.Fatalf("turn1: total=%d ratio=%.4f, want 5000 / 1.25", total, ratio)
	}
	// Compaction (provider anchor invalidated → 0): tiktoken of compacted view
	// 1600 → display 1600 × 1.25 = 2000 (calibrated, not raw tiktoken).
	total, ratio = CalibrateTotal(1600, 0, ratio)
	if total != 2000 {
		t.Fatalf("post-compaction: total=%d, want 2000 (calibrated)", total)
	}
	// Turn 2 ends on the compacted+grown context: tiktoken 2400, provider 2900 →
	// display the exact 2900, ratio nudges toward 2900/2400≈1.2083 via EMA.
	total, ratio = CalibrateTotal(2400, 2900, ratio)
	if total != 2900 {
		t.Fatalf("turn2: total=%d, want 2900 (provider exact)", total)
	}
	want := 0.5*1.25 + 0.5*(2900.0/2400.0)
	if math.Abs(ratio-want) > 1e-9 {
		t.Fatalf("turn2 ratio = %.4f, want %.4f", ratio, want)
	}
}
