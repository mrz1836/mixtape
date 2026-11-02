// Package wav writes 16-bit PCM WAV files: the minimal 44-byte RIFF/WAVE
// header (format 1, PCM) by hand with encoding/binary, no dependencies.
// Mixtape renders each recorded song to a WAV and hands it to afconvert.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

// BitsPerSample is the only sample width Mixtape records.
const BitsPerSample = 16

// HeaderSize is the size of the header WriteHeader emits.
const HeaderSize = 44

// ErrHeader reports a format or size that the 32-bit RIFF header cannot
// describe (data over ~4 GiB, or an absurd sample rate or channel count).
var ErrHeader = errors.New("wav: value does not fit the RIFF header")

// WriteHeader writes the header for dataSize bytes of interleaved PCM16LE
// audio at rate Hz with the given channel count. The caller streams exactly
// dataSize bytes of samples after it.
func WriteHeader(w io.Writer, dataSize int64, rate, channels int) error {
	blockAlign := int64(channels) * BitsPerSample / 8
	byteRate := int64(rate) * blockAlign
	riffSize := 36 + dataSize // rest of the file after the RIFF size field

	if rate <= 0 || rate > math.MaxUint32 || channels <= 0 || channels > math.MaxUint16 ||
		blockAlign <= 0 || blockAlign > math.MaxUint16 || byteRate <= 0 || byteRate > math.MaxUint32 ||
		dataSize < 0 || riffSize > math.MaxUint32 {
		return fmt.Errorf("%w: %d Hz, %d channels, %d data bytes", ErrHeader, rate, channels, dataSize)
	}

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
	binary.LittleEndian.PutUint16(h[34:36], BitsPerSample)
	copy(h[36:40], "data")
	binary.LittleEndian.PutUint32(h[40:44], uint32(dataSize))

	_, err := w.Write(h[:])
	return err
}

// Write writes a complete WAV file holding data.
func Write(w io.Writer, data []byte, rate, channels int) error {
	if err := WriteHeader(w, int64(len(data)), rate, channels); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
