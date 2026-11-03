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
// whose audio payload is the SHA-256 of the WAV, so different recordings
// hash differently and the same recording hashes the same. It satisfies
// library.Encoder and is safe for concurrent use.
type Encoder struct {
	mu   sync.Mutex
	err  error
	runs int
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
	return e.runs
}

// Encode implements library.Encoder.
func (e *Encoder) Encode(_ context.Context, wavPath, m4aPath string) error {
	e.mu.Lock()
	e.runs++
	err := e.err
	e.mu.Unlock()
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Clean(wavPath))
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	return os.WriteFile(m4aPath, M4A(M4AOptions{Audio: sum[:]}), 0o600)
}

// RealM4A encodes a short silent WAV with afconvert, the way the app does,
// and returns the M4A. It skips the test where AAC cannot be encoded.
func RealM4A(t testing.TB) []byte {
	t.Helper()
	RequireAAC(t)
	data, err := WAV(make([]byte, 44100), 44100, 2) // ~0.125 s of silence
	if err != nil {
		t.Fatalf("build WAV: %v", err)
	}
	wavPath := WriteFile(t, "in.wav", data)
	m4a := filepath.Join(filepath.Dir(wavPath), "in.m4a")
	cmd := exec.CommandContext(t.Context(), "afconvert", wavPath, m4a, "-f", "m4af", "-d", "aac") //nolint:gosec // G204: fixed tool; the paths are the test's own temp files
	if out, runErr := cmd.CombinedOutput(); runErr != nil {
		t.Fatalf("afconvert: %v: %s", runErr, out)
	}
	b, err := os.ReadFile(filepath.Clean(m4a))
	if err != nil {
		t.Fatalf("read %s: %v", m4a, err)
	}
	return b
}
