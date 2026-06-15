package runtime

import "testing"

// TestCompactionTriggerPoint pins the window-aware trigger: small windows fire
// on the absolute buffer (more headroom), large windows on the ratio.
func TestCompactionTriggerPoint(t *testing.T) {
	cases := []struct {
		name      string
		limit     int
		threshold float64
		want      int
	}{
		{"small_8k_buffer_dominates", 8000, 0.95, 6000},      // min(7600, 8000-min(13000,2000)=6000)
		{"mid_200k_buffer_slightly_earlier", 200000, 0.95, 187000}, // min(190000, 200000-13000)
		{"huge_1m_ratio_dominates", 1000000, 0.95, 950000},   // min(950000, 987000)
		{"explicit_low_threshold_wins", 8000, 0.5, 4000},     // min(4000, 6000)
		{"zero_limit", 0, 0.95, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := compactionTriggerPoint(c.limit, c.threshold); got != c.want {
				t.Errorf("compactionTriggerPoint(%d, %g) = %d, want %d", c.limit, c.threshold, got, c.want)
			}
		})
	}
}

// TestCompactionTriggerNeverLaterThanRatio: the hybrid must NEVER fire later than
// the configured ratio — only earlier (an explicit low compression_trigger still
// wins, the absolute buffer only adds headroom).
func TestCompactionTriggerNeverLaterThanRatio(t *testing.T) {
	for _, limit := range []int{4000, 8000, 32000, 128000, 200000, 1000000} {
		for _, thr := range []float64{0.5, 0.75, 0.9, 0.95, 0.97} {
			ratioPoint := int(float64(limit) * thr)
			if got := compactionTriggerPoint(limit, thr); got > ratioPoint {
				t.Errorf("limit=%d thr=%g: trigger %d > ratioPoint %d (fired LATER than the ratio)", limit, thr, got, ratioPoint)
			}
		}
	}
}

// TestCompactionTriggerSmallWindowHeadroom: a small window must keep meaningfully
// more headroom than a pure-95%% ratio would (~400 tokens on 8k) — the whole point.
func TestCompactionTriggerSmallWindowHeadroom(t *testing.T) {
	limit := 8000
	headroom := limit - compactionTriggerPoint(limit, 0.95)
	if headroom < 1000 {
		t.Errorf("8k window headroom = %d tokens, want >= 1000 (a pure 95%% ratio gives only ~400)", headroom)
	}
}
