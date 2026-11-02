//go:build darwin && cgo

package tap

import (
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestF32ToS16(t *testing.T) {
	t.Parallel()
	tests := map[float32]int16{
		0:       0,
		1:       32767,
		-1:      -32768,
		2.5:     32767,
		-7:      -32768,
		0.5:     16384, // lrintf(16383.5) rounds to even
		-0.5:    -16384,
		1e-6:    0,
		0.99999: 32767,
	}
	for in, want := range tests {
		assert.Equal(t, want, f32ToS16(in), "f32ToS16(%v)", in)
	}
	assert.Equal(t, int16(0), f32ToS16(float32(math.NaN())))
	assert.Equal(t, int16(32767), f32ToS16(float32(math.Inf(1))))
	assert.Equal(t, int16(-32768), f32ToS16(float32(math.Inf(-1))))
}

// newTestRing returns a stereo ring of capacity frames.
func newTestRing(t *testing.T, capacity uint64) *cRing {
	t.Helper()
	r, ok := newCRing(capacity, 2)
	require.True(t, ok)
	t.Cleanup(r.free)
	return r
}

func TestCRingInterleaved(t *testing.T) {
	t.Parallel()
	r := newTestRing(t, 8)

	_, _, ok := r.anchor()
	assert.False(t, ok, "no anchor before the first write")

	require.True(t, r.writeInterleaved([]float32{0.5, -0.5, 1, -1}, 2, 1000))
	frame, host, ok := r.anchor()
	require.True(t, ok)
	assert.Equal(t, uint64(0), frame)
	assert.Equal(t, uint64(1000), host)

	require.True(t, r.writeInterleaved([]float32{0, 0.25}, 2, 2000))
	frame, host, _ = r.anchor()
	assert.Equal(t, uint64(2), frame, "the anchor follows the latest write")
	assert.Equal(t, uint64(2000), host)

	got, first := r.read(10)
	assert.Equal(t, uint64(0), first)
	assert.Equal(t, []int16{16384, -16384, 32767, -32768, 0, 8192}, got)
}

func TestCRingChannelMapping(t *testing.T) {
	t.Parallel()
	mono := newTestRing(t, 8)
	require.True(t, mono.writeInterleaved([]float32{0.5, -0.5}, 1, 1))
	got, _ := mono.read(8)
	assert.Equal(t, []int16{16384, 16384, -16384, -16384}, got, "mono is duplicated to both channels")

	surround := newTestRing(t, 8)
	require.True(t, surround.writeInterleaved([]float32{0.5, -0.5, 1, 1, 0, 0.25, 1, 1}, 4, 1))
	got, _ = surround.read(8)
	assert.Equal(t, []int16{16384, -16384, 0, 8192}, got, "extra channels are dropped")
}

func TestCRingPlanar(t *testing.T) {
	t.Parallel()
	r := newTestRing(t, 8)
	require.True(t, r.writePlanar([][]float32{{0.5, 0}, {-0.5, 1}}, 42))
	got, first := r.read(8)
	assert.Equal(t, uint64(0), first)
	assert.Equal(t, []int16{16384, -16384, 0, 32767}, got)
	_, host, ok := r.anchor()
	require.True(t, ok)
	assert.Equal(t, uint64(42), host)
}

func TestCRingOverflowAndWrap(t *testing.T) {
	t.Parallel()
	r := newTestRing(t, 4)
	require.True(t, r.writeInterleaved(make([]float32, 6), 2, 1)) // 3 frames
	assert.False(t, r.writeInterleaved(make([]float32, 4), 2, 2), "2 more frames do not fit in 4")
	assert.Equal(t, uint64(1), r.takeOverflows())
	assert.Zero(t, r.takeOverflows())

	_, first := r.read(2)
	assert.Equal(t, uint64(0), first)
	// 1 frame left + 3 new = 4: wraps around the end of the buffer.
	require.True(t, r.writeInterleaved([]float32{0.5, 0.5, -0.5, -0.5, 1, 1}, 2, 3))
	got, first := r.read(8)
	assert.Equal(t, uint64(2), first)
	assert.Equal(t, []int16{0, 0, 16384, 16384, -16384, -16384, 32767, 32767}, got)

	assert.True(t, r.writeInterleaved(nil, 2, 4), "an empty write is a no-op")
}

func TestCRingConcurrent(t *testing.T) {
	t.Parallel()
	r := newTestRing(t, 256)
	const writes = 3000
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < writes; {
			v := float32(i%100) / 100
			if r.writeInterleaved([]float32{v, -v, v, -v}, 2, uint64(i)) {
				i++
			}
		}
	})
	var got []int16
	for len(got) < writes*4 {
		frames, _ := r.read(64)
		got = append(got, frames...)
		_, _, _ = r.anchor()
	}
	wg.Wait()
	for i := range writes {
		want := f32ToS16(float32(i%100) / 100)
		require.Equal(t, []int16{want, -want, want, -want}, got[i*4:i*4+4], "write %d", i)
	}
}

func TestClockPair(t *testing.T) {
	t.Parallel()
	c := sampleClock()
	now := hostNowNS()
	assert.InDelta(t, 0, time.Since(c.at(now)).Seconds(), 0.01, "the host clock maps onto Go's monotonic clock")
	assert.Equal(t, c.goTime.Add(time.Second), c.at(c.hostNS+uint64(time.Second)))
}

func TestOSStatus(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "OSStatus 'who?' (2003332927)", osStatus(0x77686f3f))
	assert.Equal(t, "OSStatus -50", osStatus(-50))
}

func TestAvailableMatchesVersion(t *testing.T) {
	t.Parallel()
	st := Available()
	if !st.Supported {
		assert.NotEmpty(t, st.Reason)
	}
}
