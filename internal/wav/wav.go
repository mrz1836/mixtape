// Package wav writes PCM WAV files: the minimal 44-byte RIFF/WAVE header
// (format 1, PCM) by hand with encoding/binary, no dependencies. Mixtape
// renders each recorded song to a WAV and hands it to afconvert.
//
// Samples are 16 or 24 bits; 24-bit samples are packed in three bytes.
// The plain PCM header (rather than WAVE_FORMAT_EXTENSIBLE) carries 24 bits
// as well: afconvert reads the depth from it, encodes ALAC "from 24-bit
// source", and the result decodes bit-exact (measured on macOS 15).
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"
)

// HeaderSize is the size of the header WriteHeader emits.
const HeaderSize = 44

// ErrHeader reports a format or size that the 32-bit RIFF header cannot
// describe (data over ~4 GiB, or an absurd sample rate or channel count),
// or a sample width other than 16 or 24 bits.
var ErrHeader = errors.New("wav: value does not fit the RIFF header")

// MaxDuration is the longest recording a WAV can hold at rate Hz with the
// given channel count and bits per sample: about 6.2 hours of 16-bit and
// 4.1 hours of 24-bit 48 kHz stereo.
func MaxDuration(rate, channels, bits int) time.Duration {
	perSecond := int64(rate) * int64(channels) * int64(bits/8)
	if perSecond <= 0 {
		return 0
	}
	return time.Duration((math.MaxUint32 - 36) / perSecond * int64(time.Second))
}

// WriteHeader writes the header for dataSize bytes of interleaved PCM
// little-endian audio at rate Hz with the given channel count and bits per
// sample (16 or 24). The caller streams exactly dataSize bytes of samples
// after it.
func WriteHeader(w io.Writer, dataSize int64, rate, channels, bits int) error {
	blockAlign := int64(channels) * int64(bits/8)
	byteRate := int64(rate) * blockAlign

	// dataSize is bounded before 36 is added to it: near MaxInt64 the sum
	// would wrap negative and pass a check on the sum.
	if (bits != 16 && bits != 24) || rate <= 0 || rate > math.MaxUint32 || channels <= 0 || channels > math.MaxUint16 ||
		blockAlign <= 0 || blockAlign > math.MaxUint16 || byteRate <= 0 || byteRate > math.MaxUint32 ||
		dataSize < 0 || dataSize > math.MaxUint32-36 {
		return fmt.Errorf("%w: %d Hz, %d channels, %d bits, %d data bytes", ErrHeader, rate, channels, bits, dataSize)
	}
	riffSize := 36 + dataSize // rest of the file after the RIFF size field

	var h [HeaderSize]byte
	copy(h[0:4], "RIFF")
	binary.LittleEndian.PutUint32(h[4:8], uint32(riffSize))
	copy(h[8:12], "WAVE")
	copy(h[12:16], "fmt ")
	binary.LittleEndian.PutUint32(h[16:20], 16) // fmt chunk size
	binary.LittleEndian.PutUint16(h[20:22], 1)  // PCM
	binary.LittleEndian.PutUint16(h[22:24], uint16(channels))
	binary.LittleEndian.PutUint32(h[24:28], uint32(rate))
	binary.LittleEndian.PutUint32(h[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(h[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(h[34:36], uint16(bits)) //nolint:gosec // G115: 16 or 24, checked above
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataSize))

	_, err := w.Write(h[:])
	return err
}
