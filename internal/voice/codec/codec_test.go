package codec

import (
	"math"
	"testing"
)

// μ-law is a lossy 8-bit companding : a round-trip must land within the segment's
// quantisation step everywhere on the int16 range. This sweeps the full range.
func TestMuLaw_RoundTripWithinQuantisation(t *testing.T) {
	for v := -32768; v <= 32767; v += 17 { // prime stride → hits every segment
		s := int16(v)
		got := MuLawDecode(MuLawEncode(s))
		diff := int32(got) - int32(s)
		if diff < 0 {
			diff = -diff
		}
		// G.711 max quantisation error grows with amplitude; 1024 covers the top
		// segment (step 256) with margin while still catching real coding bugs.
		if diff > 1024 {
			t.Fatalf("round-trip too lossy at %d: got %d (diff %d)", s, got, diff)
		}
	}
}

func TestMuLaw_KnownPoints(t *testing.T) {
	// Silence encodes to 0xFF (the classic μ-law idle pattern) and decodes back to 0.
	if c := MuLawEncode(0); c != 0xFF {
		t.Errorf("encode(0) = %#x, want 0xff", c)
	}
	if v := MuLawDecode(0xFF); v != 0 {
		t.Errorf("decode(0xff) = %d, want 0", v)
	}
	// Sign is preserved.
	if MuLawDecode(MuLawEncode(1000)) <= 0 {
		t.Error("positive sample must decode positive")
	}
	if MuLawDecode(MuLawEncode(-1000)) >= 0 {
		t.Error("negative sample must decode negative")
	}
}

func TestMuLaw_SliceHelpers(t *testing.T) {
	in := []int16{0, 100, -100, 30000, -30000}
	out := MuLawDecodeAll(MuLawEncodeAll(in))
	if len(out) != len(in) {
		t.Fatalf("length mismatch: %d vs %d", len(out), len(in))
	}
}

// Resample length math : 8k→24k triples, 24k→8k thirds, same-rate is identity.
func TestResample_Lengths(t *testing.T) {
	in := make([]int16, 160) // 20 ms @ 8 kHz
	if got := len(Resample(in, 8000, 24000)); got != 480 {
		t.Errorf("8k→24k: %d samples, want 480", got)
	}
	if got := len(Resample(make([]int16, 480), 24000, 8000)); got != 160 {
		t.Errorf("24k→8k: %d samples, want 160", got)
	}
	if out := Resample(in, 8000, 8000); &out[0] != &in[0] {
		t.Error("same-rate must return the input unchanged")
	}
}

// A 440 Hz tone resampled 8k→24k→8k must stay a 440 Hz tone : correlation with
// the original well above 0.9 (linear interpolation + box filter lose a little
// energy, never the pitch).
func TestResample_TonePreserved(t *testing.T) {
	const rate, f = 8000.0, 440.0
	in := make([]int16, 800) // 100 ms
	for i := range in {
		in[i] = int16(12000 * math.Sin(2*math.Pi*f*float64(i)/rate))
	}
	back := Resample(Resample(in, 8000, 24000), 24000, 8000)
	if len(back) != len(in) {
		t.Fatalf("round-trip length: %d vs %d", len(back), len(in))
	}
	var dot, na, nb float64
	for i := range in {
		a, b := float64(in[i]), float64(back[i])
		dot, na, nb = dot+a*b, na+a*a, nb+b*b
	}
	if corr := dot / math.Sqrt(na*nb); corr < 0.9 {
		t.Fatalf("tone degraded: correlation %.3f < 0.9", corr)
	}
}

func TestResample_Edges(t *testing.T) {
	if out := Resample(nil, 8000, 24000); len(out) != 0 {
		t.Error("nil in → nil out")
	}
	if out := Resample([]int16{5}, 8000, 24000); len(out) != 3 {
		t.Errorf("single sample upsamples to 3, got %d", len(out))
	}
	// Silence stays silence.
	for _, s := range Resample(make([]int16, 100), 24000, 8000) {
		if s != 0 {
			t.Fatal("silence must resample to silence")
		}
	}
}
