package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testRate  = 44100
	testChans = 2
)

// sine returns seconds of a 441 Hz sine, PCM16LE stereo.
func sine(seconds float64) []byte {
	n := int(seconds*testRate) * testChans * 2
	out := make([]byte, 0, n)
	for i := 0; len(out) < n; i++ {
		v := int16(20000 * math.Sin(2*math.Pi*441*float64(i)/testRate))
		b := []byte{byte(v), byte(v >> 8)}
		out = append(out, b...) // left
		out = append(out, b...) // right
	}
	return out
}

func TestWriteHeaderFields(t *testing.T) {
	t.Parallel()
	data := sine(0.1)
	var buf bytes.Buffer
	require.NoError(t, writeWAV(&buf, data, testRate, testChans))

	b := buf.Bytes()
	require.Len(t, b, HeaderSize+len(data))
	for off, sig := range map[int]string{0: "RIFF", 8: "WAVE", 12: "fmt ", 36: "data"} {
		assert.Equal(t, sig, string(b[off:off+4]), "chunk id at %d", off)
	}
	assert.Equal(t, uint32(36+len(data)), binary.LittleEndian.Uint32(b[4:8]))
	assert.Equal(t, uint16(1), binary.LittleEndian.Uint16(b[20:22]), "PCM")
	assert.Equal(t, uint16(testChans), binary.LittleEndian.Uint16(b[22:24]))
	assert.Equal(t, uint32(testRate), binary.LittleEndian.Uint32(b[24:28]))
	assert.Equal(t, uint32(testRate*testChans*2), binary.LittleEndian.Uint32(b[28:32]))
	assert.Equal(t, uint16(testChans*2), binary.LittleEndian.Uint16(b[32:34]))
	assert.Equal(t, uint16(16), binary.LittleEndian.Uint16(b[34:36]))
	assert.Equal(t, uint32(len(data)), binary.LittleEndian.Uint32(b[40:44]))
	assert.Equal(t, data, b[HeaderSize:])
}

// The 16-bit header is byte for byte what Mixtape wrote before it could
// record 24 bits.
func TestWriteHeader16BitGolden(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, WriteHeader(&buf, 1000, 48000, 2, 16))
	assert.Equal(t, []byte{
		'R', 'I', 'F', 'F', 0x0C, 0x04, 0, 0, 'W', 'A', 'V', 'E',
		'f', 'm', 't', ' ', 16, 0, 0, 0, 1, 0, 2, 0, 0x80, 0xBB, 0, 0, 0x00, 0xEE, 0x02, 0, 4, 0, 16, 0,
		'd', 'a', 't', 'a', 0xE8, 0x03, 0, 0,
	}, buf.Bytes())
}

func TestWriteHeader24Bit(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	require.NoError(t, WriteHeader(&buf, 6*44100, 44100, 2, 24))
	h := buf.Bytes()
	le := binary.LittleEndian
	assert.Equal(t, uint16(1), le.Uint16(h[20:22]), "plain PCM, which afconvert reads at 24 bits")
	assert.Equal(t, uint32(44100*6), le.Uint32(h[28:32]), "byte rate")
	assert.Equal(t, uint16(6), le.Uint16(h[32:34]), "block align: three bytes a sample")
	assert.Equal(t, uint16(24), le.Uint16(h[34:36]))
}

func TestWriteHeaderRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		size                 int64
		rate, channels, bits int
	}{
		"zero rate":      {0, 0, 2, 16},
		"zero channels":  {0, 44100, 0, 16},
		"negative size":  {-1, 44100, 2, 16},
		"too large":      {math.MaxUint32, 44100, 2, 24},
		"absurd channel": {0, 44100, math.MaxUint16 + 1, 16},
		"8 bits":         {0, 44100, 2, 8},
		"32 bits":        {0, 44100, 2, 32},
		"no bits":        {0, 44100, 2, 0},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := WriteHeader(&bytes.Buffer{}, tc.size, tc.rate, tc.channels, tc.bits)
			require.ErrorIs(t, err, ErrHeader)
		})
	}
}

type failWriter struct{}

var errWrite = errors.New("write failed")

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestWritePropagatesErrors(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, writeWAV(failWriter{}, sine(0.01), testRate, testChans), errWrite)
}

// TestAfconvertAccepts checks the header against the real consumer, at
// both depths: afconvert reads it and keeps the PCM it holds exactly.
func TestAfconvertAccepts(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	for _, tc := range []struct {
		bits   int
		format string
		data   []byte
	}{
		{16, "LEI16", sine(0.25)},
		{24, "LEI24", sine24(0.25)},
	} {
		t.Run(tc.format, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			in := filepath.Join(dir, "in.wav")
			f, err := os.Create(in)
			require.NoError(t, err)
			require.NoError(t, WriteHeader(f, int64(len(tc.data)), testRate, testChans, tc.bits))
			_, err = f.Write(tc.data)
			require.NoError(t, err)
			require.NoError(t, f.Close())

			// Reading the header is the point, so the output is plain PCM: unlike
			// AAC or ALAC, it needs no encoder the sandbox could hide.
			out := filepath.Join(dir, "out.wav")
			cmd := exec.CommandContext(t.Context(), "afconvert", in, out, "-f", "WAVE", "-d", tc.format)
			b, err := cmd.CombinedOutput()
			require.NoError(t, err, string(b))
			got, err := os.ReadFile(out)
			require.NoError(t, err)
			assert.True(t, bytes.HasSuffix(got, tc.data), "the samples come back exactly at %d bits", tc.bits)
		})
	}
}

func TestMaxDuration(t *testing.T) {
	t.Parallel()
	d := MaxDuration(48000, 2, 16)
	assert.Greater(t, d, 6*time.Hour)
	assert.Less(t, d, 6*time.Hour+15*time.Minute)
	d = MaxDuration(48000, 2, 24)
	assert.Greater(t, d, 4*time.Hour, "24-bit 48 kHz stereo: about 4.1 hours")
	assert.Less(t, d, 4*time.Hour+15*time.Minute)
	assert.Zero(t, MaxDuration(0, 2, 16))
	assert.Zero(t, MaxDuration(48000, 2, 0))
}

// sine24 is sine at 24 bits: each 16-bit sample with a low byte below it.
func sine24(seconds float64) []byte {
	s16 := sine(seconds)
	out := make([]byte, 0, len(s16)/2*3)
	for i := 0; i+1 < len(s16); i += 2 {
		out = append(out, byte(i), s16[i], s16[i+1])
	}
	return out
}

// writeWAV writes a complete 16-bit WAV file holding data, as the engine
// does with its header and the spooled audio.
func writeWAV(w io.Writer, data []byte, rate, channels int) error {
	if err := WriteHeader(w, int64(len(data)), rate, channels, 16); err != nil {
		return err
	}
	_, err := w.Write(data)
	return err
}
