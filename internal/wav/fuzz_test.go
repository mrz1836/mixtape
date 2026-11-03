package wav

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzWriteHeader checks WriteHeader on any size and format: it refuses
// with ErrHeader exactly what the 32-bit RIFF fields cannot hold, and
// otherwise writes a 44-byte header whose fields read back as the inputs.
func FuzzWriteHeader(f *testing.F) {
	f.Add(int64(len(sine(0.1))), testRate, testChans, 16)
	f.Add(int64(0), 48000, 1, 24)
	f.Add(int64(0), 0, 2, 16)
	f.Add(int64(0), 44100, 0, 16)
	f.Add(int64(-1), 44100, 2, 16)
	f.Add(int64(math.MaxUint32), 44100, 2, 16)
	f.Add(int64(math.MaxUint32-36), 44100, 2, 24)
	f.Add(int64(0), 44100, math.MaxUint16+1, 16)
	f.Add(int64(0), math.MaxUint32, 1, 16)
	f.Add(int64(0), 1, math.MaxUint16/2, 16)
	f.Add(int64(0), 1, math.MaxUint16/3, 24)
	f.Add(int64(0), 44100, 2, 32)
	// 36+dataSize once overflowed to a negative RIFF size that passed the
	// range check, and a truncated header was written.
	f.Add(int64(math.MaxInt64), 44100, 2, 16)

	f.Fuzz(func(t *testing.T, dataSize int64, rate, channels, bits int) {
		// Every product below is small enough for int64 once its factors
		// are in range, which && checks first.
		width := int64(bits / 8)
		fits := (bits == 16 || bits == 24) && rate > 0 && rate <= math.MaxUint32 &&
			channels > 0 && int64(channels)*width <= math.MaxUint16 &&
			int64(rate)*int64(channels)*width <= math.MaxUint32 &&
			dataSize >= 0 && dataSize <= math.MaxUint32-36

		var buf bytes.Buffer
		err := WriteHeader(&buf, dataSize, rate, channels, bits)
		if !fits {
			require.ErrorIs(t, err, ErrHeader, "%d bytes, %d Hz, %d channels, %d bits", dataSize, rate, channels, bits)
			assert.Zero(t, buf.Len(), "nothing is written on error")
			return
		}
		require.NoError(t, err)

		h := buf.Bytes()
		require.Len(t, h, HeaderSize)
		le := binary.LittleEndian
		for off, id := range map[int]string{0: "RIFF", 8: "WAVE", 12: "fmt ", 36: "data"} {
			assert.Equal(t, id, string(h[off:off+4]), "chunk id at %d", off)
		}
		assert.Equal(t, 36+dataSize, int64(le.Uint32(h[4:8])), "RIFF size")
		assert.Equal(t, uint32(16), le.Uint32(h[16:20]), "fmt chunk size")
		assert.Equal(t, uint16(1), le.Uint16(h[20:22]), "PCM")
		assert.Equal(t, channels, int(le.Uint16(h[22:24])), "channels")
		assert.Equal(t, int64(rate), int64(le.Uint32(h[24:28])), "rate")
		assert.Equal(t, int64(rate)*int64(channels)*width, int64(le.Uint32(h[28:32])), "byte rate")
		assert.Equal(t, int64(channels)*width, int64(le.Uint16(h[32:34])), "block align")
		assert.Equal(t, bits, int(le.Uint16(h[34:36])), "bits per sample")
		assert.Equal(t, dataSize, int64(le.Uint32(h[40:44])), "data size")
	})
}
