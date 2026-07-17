package codec

const (
	muLawBias = 0x84
	muLawClip = 32635
)

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

func MuLawDecodeAll(in []byte) []int16 {
	out := make([]int16, len(in))
	for i, c := range in {
		out[i] = MuLawDecode(c)
	}
	return out
}

func MuLawEncodeAll(in []int16) []byte {
	out := make([]byte, len(in))
	for i, s := range in {
		out[i] = MuLawEncode(s)
	}
	return out
}

func Resample(in []int16, from, to int) []int16 {
	if from == to || from <= 0 || to <= 0 || len(in) == 0 {
		return in
	}
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
