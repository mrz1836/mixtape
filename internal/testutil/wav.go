package testutil

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/mrz1836/mixtape/internal/wav"
)

// WAV returns a complete PCM16 WAV file holding pcm (interleaved 16-bit
// little-endian samples).
func WAV(pcm []byte, rate, channels int) ([]byte, error) {
	var b bytes.Buffer
	if err := wav.WriteHeader(&b, int64(len(pcm)), rate, channels); err != nil {
		return nil, err
	}
	b.Write(pcm)
	return b.Bytes(), nil
}

// Sine returns d of a stereo sine wave at freq Hz as PCM16 samples.
// Distinct frequencies give distinct audio, and so distinct audio hashes.
func Sine(freq float64, rate int, d time.Duration) []byte {
	n := int(d.Seconds() * float64(rate))
	pcm := make([]byte, 0, n*4)
	for i := range n {
		v := int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/float64(rate)))
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v)) //nolint:gosec // G115: two's complement, as PCM16 stores it
		pcm = binary.LittleEndian.AppendUint16(pcm, uint16(v)) //nolint:gosec // G115: two's complement, as PCM16 stores it
	}
	return pcm
}

// WriteFile writes data to name in a fresh temporary folder of the test
// and returns the file's path.
func WriteFile(t testing.TB, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

// SineWAV writes a short (0.2 s) 44.1 kHz stereo sine WAV at freq Hz to a
// temporary file and returns its path.
func SineWAV(t testing.TB, freq float64) string {
	t.Helper()
	data, err := WAV(Sine(freq, 44100, 200*time.Millisecond), 44100, 2)
	if err != nil {
		t.Fatalf("build WAV: %v", err)
	}
	return WriteFile(t, "rec.wav", data)
}

// Encoder stands in for afconvert: it writes a synthetic M4A (see [M4A])
// whose sound track declares Codec and whose audio payload is the SHA-256
// of the codec and the WAV, so different recordings hash differently, the
// same recording hashes the same, and the same recording in two codecs
// does not. It records every call, satisfies library.Encoder, and is safe
// for concurrent use.
type Encoder struct {
	// Codec is what the stand-in encodes to: "aac" (the zero value, as
	// with afconvert) or "alac".
	Codec string

	mu    sync.Mutex
	err   error
	calls []EncodeCall
}

// EncodeCall is one Encode the stand-in was asked for: the WAV and what
// its header says.
type EncodeCall struct {
	WAV      string
	Codec    string
	Rate     int
	Channels int
	Bits     int
}

// Fail makes every later Encode return err (nil: succeed again).
func (e *Encoder) Fail(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.err = err
}

// Runs is how many times Encode was called.
func (e *Encoder) Runs() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// Calls returns every Encode so far, in order.
func (e *Encoder) Calls() []EncodeCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.calls)
}

// codec is the codec the stand-in writes.
func (e *Encoder) codec() string {
	if e.Codec == "" {
		return "aac"
	}
	return e.Codec
}

// Encode implements library.Encoder.
func (e *Encoder) Encode(_ context.Context, wavPath, m4aPath string) error {
	data, readErr := os.ReadFile(filepath.Clean(wavPath))
	call := EncodeCall{WAV: wavPath, Codec: e.codec()}
	if len(data) >= wav.HeaderSize {
		le := binary.LittleEndian
		call.Channels, call.Rate, call.Bits = int(le.Uint16(data[22:24])), int(le.Uint32(data[24:28])), int(le.Uint16(data[34:36]))
	}
	e.mu.Lock()
	e.calls = append(e.calls, call)
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	sum := sha256.Sum256(append([]byte(call.Codec+"\x00"), data...))
	return os.WriteFile(m4aPath, M4A(M4AOptions{Audio: sum[:], Codec: call.Codec}), 0o600)
}

// RealM4A encodes a short WAV with afconvert to codec ("aac" or "alac"),
// the way the app does, and returns the M4A. It skips the test where
// afconvert cannot encode it.
func RealM4A(t testing.TB, codec string) []byte {
	t.Helper()
	RequireEncoder(t, codec)
	data, err := WAV(Sine(440, 44100, 125*time.Millisecond), 44100, 2)
	if err != nil {
		t.Fatalf("build WAV: %v", err)
	}
	wavPath := WriteFile(t, "in.wav", data)
	m4a := filepath.Join(filepath.Dir(wavPath), "in.m4a")
	cmd := exec.CommandContext(t.Context(), "afconvert", wavPath, m4a, "-f", "m4af", "-d", codec) //nolint:gosec // G204: fixed tool; the paths are the test's own temp files
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("afconvert: %v: %s", runErr, out)
	}
	b, err := os.ReadFile(filepath.Clean(m4a))
	if err != nil {
		t.Fatalf("read %s: %v", m4a, err)
	}
	return b
}
