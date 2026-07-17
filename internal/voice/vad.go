package voice

import "math"

type VAD interface {
	Push(Frame) (speech bool, endpoint bool)
	Reset()
}

type EnergyVAD struct {
	Threshold float64
	SilenceMs int

	wasSpeech bool
	silenceMs int
}

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
		return false, true
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
