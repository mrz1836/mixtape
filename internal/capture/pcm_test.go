package capture

import (
	"encoding/binary"
	"math"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFormatDepth(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		f                  Format
		bits, sample, size int
		valid              bool
	}{
		{Format{Rate: 48000, Channels: 2}, 16, 2, 4, true},
		{Format{Rate: 48000, Channels: 2, Bits: 16}, 16, 2, 4, true},
		{Format{Rate: 44100, Channels: 2, Bits: 24}, 24, 3, 6, true},
		{Format{Rate: 44100, Channels: 1, Bits: 24}, 24, 3, 3, true},
		{Format{Rate: 44100, Channels: 2, Bits: 32}, 32, 4, 8, false},
		{Format{Rate: 0, Channels: 2, Bits: 24}, 24, 3, 6, false},
		{Format{Rate: 48000, Channels: 0}, 16, 2, 0, false},
	} {
		assert.Equal(t, tc.bits, tc.f.SampleBits(), "%+v", tc.f)
		assert.Equal(t, tc.sample, tc.f.BytesPerSample(), "%+v", tc.f)
		assert.Equal(t, tc.size, tc.f.BytesPerFrame(), "%+v", tc.f)
		assert.Equal(t, tc.valid, tc.f.Valid(), "%+v", tc.f)
	}
	assert.NotEqual(t, Format{Rate: 48000, Channels: 2}, Format{Rate: 48000, Channels: 2, Bits: 16},
		"formats compare as they are, never normalized")
}

// samples16 and samples24 decode PCM back to integers.
func samples16(pcm []byte) []int32 {
	out := make([]int32, 0, len(pcm)/2)
	for i := 0; i+1 < len(pcm); i += 2 {
		out = append(out, int32(int16(binary.LittleEndian.Uint16(pcm[i:]))))
	}
	return out
}

func samples24(pcm []byte) []int32 {
	out := make([]int32, 0, len(pcm)/3)
	for i := 0; i+2 < len(pcm); i += 3 {
		out = append(out, Sample24(pcm[i:]))
	}
	return out
}

// Every integer sample a decoder turns into a float, n/2^(bits−1), comes
// back exactly: all 65536 at 16 bits, and a dense sweep at 24.
func TestConvertF32IsExactForIntegerSamples(t *testing.T) {
	t.Parallel()
	src := make([]float32, 0, 1<<16)
	want := make([]int32, 0, 1<<16)
	for n := int32(math.MinInt16); n <= math.MaxInt16; n++ {
		src = append(src, float32(n)/(1<<15))
		want = append(want, n)
	}
	assert.Equal(t, want, samples16(ConvertF32(nil, src, 16)))

	src, want = src[:0], want[:0]
	for n := int32(-1 << 23); n < 1<<23; n += 97 {
		src = append(src, float32(n)/(1<<23))
		want = append(want, n)
	}
	src, want = append(src, float32(1<<23-1)/(1<<23)), append(want, 1<<23-1)
	assert.Equal(t, want, samples24(ConvertF32(nil, src, 24)))
}

func TestConvertF32ClampsAndRounds(t *testing.T) {
	t.Parallel()
	inf, nan := float32(math.Inf(1)), float32(math.NaN())
	src := []float32{1, 1.5, inf, -1, -2, -inf, nan, 0, 0.5 / (1 << 15), 1.5 / (1 << 15), -0.5 / (1 << 15)}
	assert.Equal(t, []int32{32767, 32767, 32767, -32768, -32768, -32768, 0, 0, 0, 2, 0}, samples16(ConvertF32(nil, src, 16)),
		"clipped at full scale; NaN is silence; halves round to even")
	src = []float32{1, inf, -1, -inf, nan, 0.5 / (1 << 23), 1.5 / (1 << 23), -1.5 / (1 << 23)}
	assert.Equal(t, []int32{1<<23 - 1, 1<<23 - 1, -1 << 23, -1 << 23, 0, 0, 2, -2}, samples24(ConvertF32(nil, src, 24)))
}

func TestConvertF32Appends(t *testing.T) {
	t.Parallel()
	out := ConvertF32([]byte{0xAA}, []float32{0, 0}, 24)
	assert.Equal(t, []byte{0xAA, 0, 0, 0, 0, 0, 0}, out)
	assert.Len(t, ConvertF32(nil, []float32{0.1, 0.2, 0.3}, 16), 6)
	assert.Empty(t, ConvertF32(nil, nil, 24))
}

// The 16-bit conversion used to scale by 32767: the exact scale moves any
// sample by at most one step.
func TestConvertF32Stays16BitCompatible(t *testing.T) {
	t.Parallel()
	x := uint32(88172645)
	for range 100000 {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		f := float32(x)/math.MaxUint32*2 - 1
		old := math.Round(float64(f) * 32767)
		got := samples16(ConvertF32(nil, []float32{f}, 16))[0]
		assert.LessOrEqual(t, math.Abs(float64(got)-old), 1.0, "%v", f)
	}
}

func TestPeak(t *testing.T) {
	t.Parallel()
	f16, f24 := Format{Rate: 48000, Channels: 2}, Format{Rate: 48000, Channels: 2, Bits: 24}
	for name, tc := range map[string]struct {
		src  []float32
		want int // in 16-bit units at both depths
	}{
		"silence":     {[]float32{0, 0, 0, 0}, 0},
		"full scale":  {[]float32{1, -1}, 32768},
		"positive":    {[]float32{0.25, -0.125}, 8192},
		"negative":    {[]float32{-0.5, 0.25}, 16384},
		"the floor":   {[]float32{64.0 / 32768, 0}, 64},
		"near silent": {[]float32{1.0 / 32768, -1.0 / 32768}, 1},
	} {
		assert.Equal(t, tc.want, Peak(ConvertF32(nil, tc.src, 16), f16), "16-bit %s", name)
		assert.Equal(t, tc.want, Peak(ConvertF32(nil, tc.src, 24), f24), "24-bit %s", name)
	}
	// Below one 16-bit step, 24-bit detail reads as silence on this scale.
	assert.Zero(t, Peak(ConvertF32(nil, []float32{100.0 / (1 << 23)}, 24), f24))
	assert.Zero(t, Peak([]byte{0xFF}, f16), "a partial sample is ignored")
	assert.Zero(t, Peak([]byte{0xFF, 0xFF}, f24))
	assert.Equal(t, 32767, Peak([]byte{0xFF, 0xFF, 0x7F}, f24), "the largest positive 24-bit sample")
}

func TestSample24(t *testing.T) {
	t.Parallel()
	for _, n := range []int32{0, 1, -1, 1<<23 - 1, -1 << 23, 4660, -4660} {
		b := []byte{byte(n), byte(n >> 8), byte(n >> 16)}
		assert.Equal(t, n, Sample24(b))
	}
}

// FuzzConvertF32 converts any float: the result is in range, and turned
// back into a float it converts to itself.
func FuzzConvertF32(f *testing.F) {
	for _, x := range []float32{0, 1, -1, 0.5, -0.999, 1e-9, float32(math.NaN()), float32(math.Inf(-1)), 3.4e38} {
		f.Add(math.Float32bits(x))
	}
	f.Fuzz(func(t *testing.T, bits uint32) {
		x := math.Float32frombits(bits)
		for _, depth := range []int{16, 24} {
			pcm := ConvertF32(nil, []float32{x}, depth)
			require.Len(t, pcm, depth/8)
			var n int32
			if depth == 24 {
				n = Sample24(pcm)
			} else {
				n = samples16(pcm)[0]
			}
			full := int32(1) << (depth - 1)
			require.GreaterOrEqual(t, n, -full)
			require.Less(t, n, full)
			again := ConvertF32(nil, []float32{float32(n) / float32(full)}, depth)
			assert.Equal(t, pcm, again, "%v at %d bits", x, depth)
		}
	})
}

// BenchmarkPeak measures the loudness check every captured block passes
// through on the capture goroutine (10 ms of 48 kHz stereo).
func BenchmarkPeak(b *testing.B) {
	for _, bits := range []int{16, 24} {
		b.Run(strconv.Itoa(bits)+"-bit", func(b *testing.B) {
			f := Format{Rate: 48000, Channels: 2, Bits: bits}
			pcm := make([]byte, 480*f.BytesPerFrame())
			for i := range pcm {
				pcm[i] = byte(i)
			}
			b.SetBytes(int64(len(pcm)))
			b.ReportAllocs()
			for b.Loop() {
				_ = Peak(pcm, f)
			}
		})
	}
}

// BenchmarkConvertF32 measures the conversion of one 10 ms Core Audio
// buffer of 48 kHz stereo.
func BenchmarkConvertF32(b *testing.B) {
	src := make([]float32, 960)
	for i := range src {
		src[i] = float32(math.Sin(float64(i) / 10))
	}
	for _, bits := range []int{16, 24} {
		b.Run(strconv.Itoa(bits)+"-bit", func(b *testing.B) {
			dst := make([]byte, 0, len(src)*3)
			b.SetBytes(int64(len(src) * 4))
			b.ReportAllocs()
			for b.Loop() {
				dst = ConvertF32(dst[:0], src, bits)
			}
		})
	}
}
