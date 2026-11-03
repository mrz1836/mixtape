package capture

import (
	"encoding/binary"
	"math"
)

// ConvertF32 appends the Float32 samples of src to dst as signed
// little-endian PCM of bits bits (16, or 24 packed in three bytes) and
// returns the extended slice.
//
// A sample is scaled by exactly 2^(bits−1), rounded to the nearest
// integer (halves to even) and clamped to the integer range, so +1.0 and
// beyond clip to the largest value and NaN becomes 0. The scale is a power
// of two: a sample a decoder made from an integer of that width, x =
// n/2^(bits−1), comes back as n exactly. (Scaling by 2^(bits−1)−1 instead,
// as many converters do, would move such a sample by up to one step.)
func ConvertF32(dst []byte, src []float32, bits int) []byte {
	if bits == Bits24 {
		return convert(dst, src, 1<<23, func(b []byte, v int32) []byte {
			return append(b, byte(v), byte(v>>8), byte(v>>16)) //nolint:gosec // G115: the three low bytes of a clamped 24-bit sample
		})
	}
	return convert(dst, src, 1<<15, func(b []byte, v int32) []byte {
		return binary.LittleEndian.AppendUint16(b, uint16(int16(v))) //nolint:gosec // G115: clamped to int16 range by convert
	})
}

// convert scales, rounds and clamps each sample to [−full, full−1] and
// appends it with put.
func convert(dst []byte, src []float32, full int32, put func([]byte, int32) []byte) []byte {
	scale, hi, lo := float64(full), float64(full-1), float64(-full)
	for _, x := range src {
		v := math.RoundToEven(float64(x) * scale)
		switch {
		case v != v: // NaN
			v = 0
		case v > hi:
			v = hi
		case v < lo:
			v = lo
		}
		dst = put(dst, int32(v))
	}
	return dst
}

// Peak returns the largest sample magnitude in pcm, in 16-bit units
// whatever the format's depth: a 24-bit magnitude is shifted down by 8
// bits. So one silence floor and one set of loudness thresholds serve every
// depth. A trailing partial sample is ignored.
func Peak(pcm []byte, f Format) int {
	m := 0
	if f.SampleBits() == Bits24 {
		for i := 0; i+2 < len(pcm); i += 3 {
			v := int32(uint32(pcm[i]) | uint32(pcm[i+1])<<8 | uint32(pcm[i+2])<<16) //nolint:gosec // G115: 24 bits fit an int32
			if v >= 1<<23 {
				v = 1<<24 - v // two's complement: the magnitude of a negative sample
			}
			m = max(m, int(v>>8))
		}
		return m
	}
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(binary.LittleEndian.Uint16(pcm[i:]))
		if v >= 1<<15 {
			v = 1<<16 - v
		}
		m = max(m, v)
	}
	return m
}

// Sample24 reads the packed 24-bit sample at b[0:3] as a signed value.
func Sample24(b []byte) int32 {
	return int32(uint32(b[0])|uint32(b[1])<<8|uint32(b[2])<<16) << 8 >> 8 //nolint:gosec // G115: sign extension of 24 bits
}
