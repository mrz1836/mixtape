//go:build darwin && cgo

package device

import (
	"bytes"
	"encoding/binary"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

func seq(start, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(start + i)
	}
	return b
}

func TestRingWriteRead(t *testing.T) {
	t.Parallel()
	r := newRing(16, 8)
	t0 := time.Now()

	require.True(t, r.write(seq(0, 6), t0))
	require.True(t, r.write(seq(6, 6), t0.Add(time.Second)))

	dst := make([]byte, 4)
	n, o := r.readInto(dst)
	require.True(t, o.ok)
	assert.Equal(t, 4, n)
	assert.Equal(t, seq(0, 4), dst)
	assert.Equal(t, t0, o.at)
	assert.Zero(t, o.offset)

	// The next read starts mid-way through the first write.
	dst = make([]byte, 16)
	n, o = r.readInto(dst)
	require.True(t, o.ok)
	assert.Equal(t, 8, n)
	assert.Equal(t, seq(4, 8), dst[:n])
	assert.Equal(t, t0, o.at)
	assert.Equal(t, int64(4), o.offset, "offset past the covering stamp")

	n, o = r.readInto(dst)
	assert.Zero(t, n)
	assert.False(t, o.ok)
}

func TestRingWrapsAround(t *testing.T) {
	t.Parallel()
	r := newRing(10, 8)
	t0 := time.Now()
	dst := make([]byte, 10)

	for i := range 20 {
		require.True(t, r.write(seq(i*7, 7), t0.Add(time.Duration(i)*time.Millisecond)))
		n, o := r.readInto(dst)
		require.True(t, o.ok)
		require.Equal(t, 7, n)
		require.Equal(t, seq(i*7, 7), dst[:n], "write %d survives the wrap", i)
		require.Equal(t, t0.Add(time.Duration(i)*time.Millisecond), o.at)
		require.Zero(t, o.offset)
	}
}

func TestRingOverflowDropsWholeWrites(t *testing.T) {
	t.Parallel()
	r := newRing(8, 8)
	now := time.Now()
	require.True(t, r.write(seq(0, 6), now))
	assert.False(t, r.write(seq(6, 6), now), "no room for the second write")
	assert.Equal(t, int64(1), r.takeOverflows())
	assert.Zero(t, r.takeOverflows(), "the count resets")

	dst := make([]byte, 8)
	n, _ := r.readInto(dst)
	assert.Equal(t, seq(0, 6), dst[:n], "the stored write is intact")

	full := newRing(64, 2)
	require.True(t, full.write(seq(0, 1), now))
	require.True(t, full.write(seq(1, 1), now))
	assert.False(t, full.write(seq(2, 1), now), "a full stamp table also counts as overflow")
	assert.True(t, full.write(nil, now), "an empty write always succeeds")
}

func TestRingStampsSurviveSplitReads(t *testing.T) {
	t.Parallel()
	r := newRing(64, 8)
	t0 := time.Now()
	require.True(t, r.write(seq(0, 10), t0))
	require.True(t, r.write(seq(10, 10), t0.Add(10*time.Millisecond)))

	dst := make([]byte, 15)
	_, o := r.readInto(dst) // bytes 0–14
	require.True(t, o.ok)
	assert.Equal(t, t0, o.at)
	assert.Zero(t, o.offset)

	_, o = r.readInto(dst) // bytes 15–19, inside the second write
	require.True(t, o.ok)
	assert.Equal(t, t0.Add(10*time.Millisecond), o.at)
	assert.Equal(t, int64(5), o.offset)
}

func TestRingConcurrent(t *testing.T) {
	t.Parallel()
	r := newRing(256, 64)
	const writes = 5000
	var produced bytes.Buffer
	var wg sync.WaitGroup
	var mu sync.Mutex
	wg.Go(func() {
		for i := 0; i < writes; {
			p := seq(i*3, 3)
			if r.write(p, time.Now()) {
				mu.Lock()
				produced.Write(p)
				mu.Unlock()
				i++
			}
		}
	})

	var consumed bytes.Buffer
	dst := make([]byte, 50)
	for consumed.Len() < writes*3 {
		n, _ := r.readInto(dst)
		consumed.Write(dst[:n])
	}
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, produced.Bytes(), consumed.Bytes(), "every published byte arrives once, in order")
}

func TestOriginCapturedAt(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: 48000, Channels: 2, Bits: 24}
	at := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	const frame = 2 * 4                                // stereo Float32 in the ring
	o := origin{at: at, offset: 480 * frame, ok: true} // 480 frames past the stamp
	assert.Equal(t, at.Add(10*time.Millisecond), o.capturedAt(f, frame), "frames are counted in the ring's own size")
	assert.Equal(t, at, origin{at: at, ok: true}.capturedAt(f, frame), "the stamped byte itself")
	assert.True(t, origin{at: at}.capturedAt(f, frame).IsZero(), "no stamp, no time")
	assert.True(t, o.capturedAt(f, 0).IsZero(), "no frame size, no time")
}

// What the drain hands on is exactly what the device produced: Float32
// through the ring, decoded, then converted at the session's depth.
func TestRingCarriesFloatsToExactPCM(t *testing.T) {
	t.Parallel()
	in := []float32{0, 0.5, -0.5, float32(8388607) / (1 << 23), -1, 1.5, float32(1234567) / (1 << 23), float32(-42) / (1 << 23)}
	raw := make([]byte, 0, len(in)*ringSampleBytes)
	for _, x := range in {
		raw = binary.LittleEndian.AppendUint32(raw, math.Float32bits(x))
	}
	r := newRing(len(raw)*2, 8)
	require.True(t, r.write(raw, time.Now()))
	buf := make([]byte, len(raw)*2)
	n, _ := r.readInto(buf)
	require.Equal(t, len(raw), n)
	got := floats(make([]float32, len(in)), buf[:n])
	assert.Equal(t, in, got)
	pcm := capture.ConvertF32(nil, got, 24)
	want := []int32{0, 1 << 22, -(1 << 22), 1<<23 - 1, -(1 << 23), 1<<23 - 1, 1234567, -42}
	for i, w := range want {
		assert.Equal(t, w, capture.Sample24(pcm[i*3:]), "sample %d", i)
	}
}
