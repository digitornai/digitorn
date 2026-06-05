package contextsvc

import "math"

// calibClampLo and calibClampHi bound the learned tokenizer→provider ratio so a
// single freak count can never wreck the gauge. Real provider/tiktoken ratios
// sit well inside this band for every provider.
const (
	calibClampLo = 0.25
	calibClampHi = 4.0
)

// CalibrateTotal converts a raw background-tokenizer (tiktoken) count into THIS
// session's provider REAL token count, provider-agnostically, via a ratio
// learned from the provider's own reported usage. Nothing is hard-coded per
// provider — the ratio is whatever this session's provider counts vs the local
// tokenizer.
//
//   - providerAnchor > 0 : a FRESH exact provider count for the SAME content is
//     available (a turn just ended). (Re)learn the ratio (EMA-smoothed) AND
//     return the provider count verbatim — so the displayed total is EXACT at
//     every turn boundary, for every provider.
//   - providerAnchor == 0 : no fresh anchor (between turns, or right after a
//     compaction the provider hasn't seen). Apply the learned ratio to the raw
//     tokenizer count — the closest provider-calibrated estimate.
//   - oldRatio <= 0 : not yet learned → ratio 1.0 (raw passthrough) until the
//     first turn teaches it.
//
// Pure + deterministic : the whole calibration guarantee is proven by testing
// this function directly.
func CalibrateTotal(raw, providerAnchor int, oldRatio float64) (total int, ratio float64) {
	ratio = oldRatio
	if providerAnchor > 0 && raw > 0 {
		inst := float64(providerAnchor) / float64(raw)
		if inst < calibClampLo {
			inst = calibClampLo
		} else if inst > calibClampHi {
			inst = calibClampHi
		}
		if ratio <= 0 {
			ratio = inst // first calibration : take it outright
		} else {
			ratio = 0.5*ratio + 0.5*inst // EMA : absorb drift without jitter
		}
		// A fresh provider count IS the ground truth for the current content —
		// display it exactly, regardless of the (smoothed) ratio.
		return providerAnchor, ratio
	}
	if ratio <= 0 {
		ratio = 1.0
	}
	if raw <= 0 {
		return 0, ratio
	}
	return int(math.Round(float64(raw) * ratio)), ratio
}
