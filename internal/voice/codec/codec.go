// Package codec holds the telephony audio primitives the voice transports need:
// G.711 μ-law (the PSTN / Twilio Media Streams wire format, 8 kHz mono) ⇄ linear
// PCM16, and a small linear resampler between the call rate (8 kHz) and the
// provider rate (e.g. 24 kHz). Pure Go, allocation-light, no dependencies — these
// run per 20 ms frame on the media path, so they must be boring and fast.
package codec

// G.711 μ-law constants (ITU-T G.711).
const (
	muLawBias = 0x84
	muLawClip = 32635
)

// MuLawEncode compresses one linear PCM16 sample to its 8-bit μ-law code.
func MuLawEncode(s int16) byte {
	sign := byte(0)
	v := int32(s)
	if v < 0 {
		v = -v
		sign = 0x80
	}
	if v > muLawClip {
		v = muLawClip
	}
	v += muLawBias
	seg := byte(7)
	for mask := int32(0x4000); mask != 0 && v&mask == 0; mask >>= 1 {
		seg--
	}
	low := byte((v >> (seg + 3)) & 0x0F)
	return ^(sign | (seg << 4) | low)
}

// MuLawDecode expands one 8-bit μ-law code back to linear PCM16.
func MuLawDecode(c byte) int16 {
	c = ^c
	sign := c & 0x80
	seg := (c >> 4) & 0x07
	low := c & 0x0F
	v := (int32(low)<<3 + muLawBias) << seg
	v -= muLawBias
	if sign != 0 {
		v = -v
	}
	return int16(v)
}

// MuLawDecodeAll expands a μ-law payload (one byte per sample) to PCM16.
func MuLawDecodeAll(in []byte) []int16 {
	out := make([]int16, len(in))
	for i, c := range in {
		out[i] = MuLawDecode(c)
	}
	return out
}

// MuLawEncodeAll compresses PCM16 samples to a μ-law payload.
func MuLawEncodeAll(in []int16) []byte {
	out := make([]byte, len(in))
	for i, s := range in {
		out[i] = MuLawEncode(s)
	}
	return out
}

// Resample converts PCM16 samples from one rate to another by linear
// interpolation. Telephony-grade : it preserves speech intelligibility for the
// 8 kHz ⇄ 24 kHz hops the realtime providers need; it is not a mastering-grade
// polyphase filter and doesn't try to be. from==to (or empty input) returns the
// input unchanged.
func Resample(in []int16, from, to int) []int16 {
	if from == to || from <= 0 || to <= 0 || len(in) == 0 {
		return in
	}
	// Downsampling first runs a tiny box low-pass sized to the decimation factor,
	// which knocks down the worst aliasing without a real filter bank.
	src := in
	if to < from {
		k := from / to
		if k > 1 {
			src = boxLowPass(in, k)
		}
	}
	n := int(int64(len(src)) * int64(to) / int64(from))
	if n <= 0 {
		return nil
	}
	out := make([]int16, n)
	step := float64(from) / float64(to)
	for i := range out {
		pos := float64(i) * step
		j := int(pos)
		if j >= len(src)-1 {
			out[i] = src[len(src)-1]
			continue
		}
		frac := pos - float64(j)
		a, b := float64(src[j]), float64(src[j+1])
		out[i] = int16(a + (b-a)*frac)
	}
	return out
}

// boxLowPass is a centered moving average of width k — the cheapest usable
// anti-aliasing pre-filter for integer decimation. Deliberately the direct
// O(n·k) form : k is the decimation factor (3 for 24→8 kHz) and frames are tiny
// (≈160–480 samples), so clarity beats a sliding-window micro-optimisation.
func boxLowPass(in []int16, k int) []int16 {
	if k <= 1 || len(in) == 0 {
		return in
	}
	out := make([]int16, len(in))
	half := k / 2
	for i := range in {
		var sum, cnt int64
		for j := i - half; j <= i+half; j++ {
			if j < 0 || j >= len(in) {
				continue
			}
			sum += int64(in[j])
			cnt++
		}
		out[i] = int16(sum / cnt)
	}
	return out
}
