package voice

import "math"

// VAD is voice-activity detection / endpointing. Push reports whether the frame
// contains speech and whether an utterance just ended (trailing silence after
// speech). The orchestrator uses `speech` for barge-in and `endpoint` to commit a
// turn. Pluggable so a provider-grade VAD can replace the default.
type VAD interface {
	Push(Frame) (speech bool, endpoint bool)
	Reset()
}

// EnergyVAD is a simple RMS-energy endpointer: a frame is speech when its energy
// crosses Threshold; an utterance ends after SilenceMs of trailing silence. Good
// enough for the core; swap for WebRTC-VAD / a provider VAD when tuning latency.
type EnergyVAD struct {
	Threshold float64 // RMS threshold (PCM16 scale, ~500–1500 typical)
	SilenceMs int     // trailing silence to declare end-of-utterance

	wasSpeech bool
	silenceMs int
}

// NewEnergyVAD builds a VAD with sane voice defaults.
func NewEnergyVAD() *EnergyVAD { return &EnergyVAD{Threshold: 700, SilenceMs: 400} }

func (v *EnergyVAD) Reset() {
	v.wasSpeech = false
	v.silenceMs = 0
}

func (v *EnergyVAD) Push(f Frame) (bool, bool) {
	speech := rms(f.Samples) >= v.Threshold
	if speech {
		v.wasSpeech = true
		v.silenceMs = 0
		return true, false
	}
	if !v.wasSpeech {
		return false, false
	}
	v.silenceMs += frameMs(f)
	if v.silenceMs >= v.SilenceMs {
		v.wasSpeech = false
		v.silenceMs = 0
		return false, true // endpoint
	}
	return false, false
}

func rms(s []int16) float64 {
	if len(s) == 0 {
		return 0
	}
	var sum float64
	for _, x := range s {
		sum += float64(x) * float64(x)
	}
	return math.Sqrt(sum / float64(len(s)))
}

func frameMs(f Frame) int {
	if f.Rate <= 0 {
		return 20
	}
	return len(f.Samples) * 1000 / f.Rate
}
