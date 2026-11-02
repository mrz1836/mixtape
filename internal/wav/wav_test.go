package wav

import (
	"bytes"
	"encoding/binary"
	"errors"
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
	require.NoError(t, Write(&buf, data, testRate, testChans))

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
	assert.Equal(t, uint16(BitsPerSample), binary.LittleEndian.Uint16(b[34:36]))
	assert.Equal(t, uint32(len(data)), binary.LittleEndian.Uint32(b[40:44]))
	assert.Equal(t, data, b[HeaderSize:])
}

func TestWriteHeaderRejects(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		size           int64
		rate, channels int
	}{
		"zero rate":      {0, 0, 2},
		"zero channels":  {0, 44100, 0},
		"negative size":  {-1, 44100, 2},
		"too large":      {math.MaxUint32, 44100, 2},
		"absurd channel": {0, 44100, math.MaxUint16 + 1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := WriteHeader(&bytes.Buffer{}, tc.size, tc.rate, tc.channels)
			require.ErrorIs(t, err, ErrHeader)
		})
	}
}

type failWriter struct{}

var errWrite = errors.New("write failed")

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

func TestWritePropagatesErrors(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, Write(failWriter{}, sine(0.01), testRate, testChans), errWrite)
}

// TestAfconvertAccepts checks the header against the real consumer.
func TestAfconvertAccepts(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	dir := t.TempDir()
	in := filepath.Join(dir, "in.wav")
	f, err := os.Create(in)
	require.NoError(t, err)
	require.NoError(t, Write(f, sine(0.25), testRate, testChans))
	require.NoError(t, f.Close())

	// Reading the header is the point, so the output is plain PCM: unlike
	// AAC, it needs no encoder the sandbox could hide.
	out := filepath.Join(dir, "out.caf")
	cmd := exec.CommandContext(t.Context(), "afconvert", in, out, "-f", "caff", "-d", "LEI16")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	info, err := os.Stat(out)
	require.NoError(t, err)
	assert.Positive(t, info.Size())
}

func TestMaxDuration(t *testing.T) {
	t.Parallel()
	d := MaxDuration(48000, 2)
	assert.Greater(t, d, 6*time.Hour)
	assert.Less(t, d, 6*time.Hour+15*time.Minute)
	assert.Zero(t, MaxDuration(0, 2))
}
