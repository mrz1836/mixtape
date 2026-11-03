package spool

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

// smallRate makes chunks tiny: at 100 Hz mono a 30-second chunk holds 3000
// frames.
const smallRate = 100

func smallFormat() capture.Format { return capture.Format{Rate: smallRate, Channels: 1} }

// newSpool opens a spool in dir for the test and closes it when the test
// ends.
func newSpool(t *testing.T, dir string, f capture.Format) *Spool {
	t.Helper()
	s, err := Open(dir, f)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// frames returns n frames whose samples encode their own frame index, so
// any read can be checked for exactly which frames it returned.
func frames(f capture.Format, start, n int64) []byte {
	out := make([]byte, 0, n*int64(f.BytesPerFrame()))
	for i := start; i < start+n; i++ {
		for range f.Channels {
			out = binary.LittleEndian.AppendUint16(out, uint16(i%65536))
		}
	}
	return out
}

// frameIndexes decodes the first channel of each frame back to its index.
func frameIndexes(f capture.Format, pcm []byte) []int64 {
	bpf := f.BytesPerFrame()
	out := make([]int64, 0, len(pcm)/bpf)
	for off := 0; off+bpf <= len(pcm); off += bpf {
		out = append(out, int64(binary.LittleEndian.Uint16(pcm[off:])))
	}
	return out
}

func appendSeconds(t *testing.T, s *Spool, start time.Time, seconds int) {
	t.Helper()
	f := s.Format()
	for sec := range seconds {
		first := s.Frames()
		require.NoError(t, s.Append(capture.Block{
			PCM:    frames(f, first, int64(f.Rate)),
			Frames: f.Rate,
			At:     start.Add(time.Duration(sec) * time.Second),
		}))
	}
}

func chunkFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), chunkPrefix) {
			names = append(names, e.Name())
		}
	}
	return names
}

func TestOpenValidates(t *testing.T) {
	t.Parallel()
	_, err := Open(t.TempDir(), capture.Format{})
	require.ErrorIs(t, err, ErrFormat)
	_, err = Open(t.TempDir(), capture.Format{Rate: 48000})
	require.ErrorIs(t, err, ErrFormat)
}

func TestOpenRemovesStaleChunks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stale := filepath.Join(dir, "chunk-000007.pcm")
	other := filepath.Join(dir, "keep.txt")
	require.NoError(t, os.WriteFile(stale, []byte("old"), 0o600))
	require.NoError(t, os.WriteFile(other, []byte("x"), 0o600))

	newSpool(t, dir, smallFormat())

	assert.NoFileExists(t, stale)
	assert.FileExists(t, other, "only chunk files are removed")
}

func TestAppendAcrossChunksAndReadBack(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newSpool(t, dir, smallFormat())

	start := time.Now()
	appendSeconds(t, s, start, 75) // 7500 frames: three chunks (3000, 3000, 1500)

	assert.Equal(t, int64(7500), s.Frames())
	assert.Equal(t, int64(0), s.Oldest())
	assert.Len(t, chunkFiles(t, dir), 3)

	var buf bytes.Buffer
	n, err := s.ReadRange(2950, 3050, &buf) // straddles the first chunk boundary
	require.NoError(t, err)
	assert.Equal(t, int64(100), n)
	got := frameIndexes(smallFormat(), buf.Bytes())
	require.Len(t, got, 100)
	assert.Equal(t, int64(2950), got[0])
	assert.Equal(t, int64(3049), got[99])
	for i := 1; i < len(got); i++ {
		require.Equal(t, got[i-1]+1, got[i], "frames are contiguous")
	}
}

func TestBlockSplitAcrossChunk(t *testing.T) {
	t.Parallel()
	s := newSpool(t, t.TempDir(), smallFormat())

	// One 4000-frame block fills the first chunk and spills into the next.
	require.NoError(t, s.Append(capture.Block{PCM: frames(smallFormat(), 0, 4000), Frames: 4000, At: time.Now()}))
	var buf bytes.Buffer
	n, err := s.ReadRange(0, 4000, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(4000), n)
	got := frameIndexes(smallFormat(), buf.Bytes())
	assert.Equal(t, int64(2999), got[2999])
	assert.Equal(t, int64(3000), got[3000])
	assert.Equal(t, int64(3999), got[3999])
}

func TestReadRangeClips(t *testing.T) {
	t.Parallel()
	s := newSpool(t, t.TempDir(), smallFormat())
	appendSeconds(t, s, time.Now(), 2)

	var buf bytes.Buffer
	n, err := s.ReadRange(-50, 10_000, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(200), n, "clipped to what was captured")
	assert.Len(t, buf.Bytes(), 200*smallFormat().BytesPerFrame())

	buf.Reset()
	n, err = s.ReadRange(150, 100, &buf)
	require.NoError(t, err)
	assert.Zero(t, n, "an empty range reads nothing")
}

func TestStereoFrames(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: 48000, Channels: 2}
	s := newSpool(t, t.TempDir(), f)

	require.NoError(t, s.Append(capture.Block{PCM: frames(f, 0, 512), Frames: 512, At: time.Now()}))
	var buf bytes.Buffer
	n, err := s.ReadRange(100, 200, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(100), n)
	assert.Equal(t, frames(f, 100, 100), buf.Bytes())
}

func TestAppendRejectsShortBlock(t *testing.T) {
	t.Parallel()
	s := newSpool(t, t.TempDir(), smallFormat())
	require.ErrorIs(t, s.Append(capture.Block{PCM: make([]byte, 10), Frames: 6}), ErrShortBlock)
	require.NoError(t, s.Append(capture.Block{}), "an empty block is a no-op")
	assert.Zero(t, s.Frames())
}

func TestFrameAtInterpolates(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: 1000, Channels: 1}
	s := newSpool(t, t.TempDir(), f)

	// 100 ms blocks whose timestamps jitter by up to ±3 ms.
	start := time.Now()
	jitterMS := []int{0, 3, -2, 1, -3, 2, 0, -1, 3, -2}
	for i := range 50 {
		at := start.Add(time.Duration(i)*100*time.Millisecond + time.Duration(jitterMS[i%len(jitterMS)])*time.Millisecond)
		require.NoError(t, s.Append(capture.Block{PCM: frames(f, s.Frames(), 100), Frames: 100, At: at}))
	}

	for _, sec := range []float64{0, 0.25, 1, 2.5, 4.9} {
		at := start.Add(time.Duration(sec * float64(time.Second)))
		assert.InDelta(t, sec*1000, float64(s.FrameAt(at)), 5, "FrameAt(%.2fs)", sec)
		assert.InDelta(t, 0, timeAt(s, s.FrameAt(at)).Sub(at).Seconds(), 0.005, "timeAt inverts FrameAt at %.2fs", sec)
	}

	// Beyond the anchors both directions extrapolate at the nominal rate.
	assert.InDelta(t, -1000, float64(s.FrameAt(start.Add(-time.Second))), 5)
	last := start.Add(49 * 100 * time.Millisecond)
	assert.InDelta(t, 4900+2000, float64(s.FrameAt(last.Add(2*time.Second))), 5)
	assert.InDelta(t, 0, timeAt(s, -1000).Sub(start.Add(-time.Second)).Seconds(), 0.005)
}

func TestFrameAtThinsAndIgnoresBackwardTime(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: 1000, Channels: 1}
	s := newSpool(t, t.TempDir(), f)

	start := time.Now()
	for i := range 100 { // 10 ms blocks: thinned to one anchor per 50 ms
		require.NoError(t, s.Append(capture.Block{
			PCM: frames(f, s.Frames(), 10), Frames: 10,
			At: start.Add(time.Duration(i) * 10 * time.Millisecond),
		}))
	}
	assert.Len(t, s.anchors, 20)

	before := len(s.anchors)
	require.NoError(t, s.Append(capture.Block{PCM: frames(f, s.Frames(), 10), Frames: 10, At: start}))
	require.NoError(t, s.Append(capture.Block{PCM: frames(f, s.Frames(), 10), Frames: 10}))
	assert.Len(t, s.anchors, before, "backward and zero timestamps add no anchor")
}

func TestFrameAtWithoutAnchors(t *testing.T) {
	t.Parallel()
	s := newSpool(t, t.TempDir(), smallFormat())
	assert.Zero(t, s.FrameAt(time.Now()))
	assert.True(t, timeAt(s, 10).IsZero())

	at := time.Now()
	require.NoError(t, s.Append(capture.Block{PCM: frames(smallFormat(), 0, 100), Frames: 100, At: at}))
	assert.Equal(t, int64(50), s.FrameAt(at.Add(500*time.Millisecond)), "one anchor extrapolates")
}

func TestPrune(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s := newSpool(t, dir, smallFormat())
	start := time.Now()
	appendSeconds(t, s, start, 75) // chunks [0,3000) [3000,6000) [6000,7500)

	require.NoError(t, s.Prune(2999))
	assert.Equal(t, int64(0), s.Oldest(), "a chunk still partly needed is kept")

	require.NoError(t, s.Prune(3500))
	assert.Equal(t, int64(3000), s.Oldest())
	assert.Len(t, chunkFiles(t, dir), 2)
	assert.InDelta(t, 3500, float64(s.FrameAt(start.Add(35*time.Second))), 1, "mapping still accurate at the boundary")
	assert.LessOrEqual(t, s.anchors[0].frame, int64(3500))

	var buf bytes.Buffer
	n, err := s.ReadRange(0, 3100, &buf)
	require.NoError(t, err)
	assert.Equal(t, int64(100), n, "pruned frames are no longer readable")
	assert.Equal(t, int64(3000), frameIndexes(smallFormat(), buf.Bytes())[0])

	require.NoError(t, s.Prune(1_000_000))
	assert.Equal(t, int64(6000), s.Oldest(), "the chunk being written is never pruned")
	assert.Len(t, chunkFiles(t, dir), 1)

	appendSeconds(t, s, start.Add(75*time.Second), 1)
	assert.Equal(t, int64(7600), s.Frames(), "appending continues after a prune")
}

func TestCloseRemovesFilesAndIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, smallFormat())
	require.NoError(t, err)
	appendSeconds(t, s, time.Now(), 40)
	require.NotEmpty(t, chunkFiles(t, dir))

	require.NoError(t, s.Close())
	require.NoError(t, s.Close())
	assert.Empty(t, chunkFiles(t, dir))
	require.ErrorIs(t, s.Append(capture.Block{PCM: frames(smallFormat(), 0, 1), Frames: 1}), ErrClosed)
	_, err = s.ReadRange(0, 1, &bytes.Buffer{})
	require.ErrorIs(t, err, ErrClosed)
	require.ErrorIs(t, s.Prune(1), ErrClosed)
}

func TestConcurrentAppendAndRead(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: 1000, Channels: 2}
	s := newSpool(t, t.TempDir(), f)

	const blocks = 400 // 100 s of 250-frame blocks: several 30 000-frame chunks
	start := time.Now()
	var (
		wg        sync.WaitGroup
		appendErr error // written by the appender, read after wg.Wait
	)
	written := make(chan struct{})
	wg.Go(func() {
		defer close(written)
		for i := range blocks {
			first := int64(i) * 250
			if err := s.Append(capture.Block{
				PCM: frames(f, first, 250), Frames: 250,
				At: start.Add(time.Duration(i) * 250 * time.Millisecond),
			}); err != nil {
				appendErr = err
				return
			}
		}
	})
	deadline := time.Now().Add(30 * time.Second)
	for finished := false; !finished; {
		select {
		case <-written:
			finished = true // one last read of everything appended
		default:
		}
		require.False(t, time.Now().After(deadline), "the appender did not finish")
		end := s.Frames()
		if end > 2000 {
			var buf bytes.Buffer
			n, readErr := s.ReadRange(end-2000, end, &buf)
			require.NoError(t, readErr)
			require.Equal(t, int64(2000), n)
			got := frameIndexes(f, buf.Bytes())
			require.Equal(t, (end-2000)%65536, got[0])
			require.Equal(t, (end-1)%65536, got[len(got)-1])
			_ = s.FrameAt(start.Add(time.Second))
		}
	}
	wg.Wait()
	require.NoError(t, appendErr)
	assert.Equal(t, int64(blocks*250), s.Frames())
}

func TestPruneRetriesChunksItCouldNotDelete(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root deletes from read-only folders")
	}
	dir := t.TempDir()
	f := smallFormat()
	s := newSpool(t, dir, f)
	require.NoError(t, s.Append(capture.Block{PCM: frames(f, 0, 7000), Frames: 7000, At: time.Now()}))
	require.Len(t, chunkFiles(t, dir), 3)

	require.NoError(t, os.Chmod(dir, 0o500)) // deletions now fail
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	require.Error(t, s.Prune(6000))
	assert.Len(t, chunkFiles(t, dir), 3, "nothing could be deleted")
	assert.Equal(t, int64(6000), s.Oldest(), "the pruned chunks are gone from the spool all the same")

	require.NoError(t, os.Chmod(dir, 0o700))
	require.NoError(t, s.Prune(6000), "the next prune deletes what the last one could not")
	assert.Len(t, chunkFiles(t, dir), 1)
}

func TestCloseDeletesChunksPruneCouldNotDelete(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root deletes from read-only folders")
	}
	dir := t.TempDir()
	f := smallFormat()
	s, err := Open(dir, f)
	require.NoError(t, err)
	require.NoError(t, s.Append(capture.Block{PCM: frames(f, 0, 7000), Frames: 7000, At: time.Now()}))
	require.NoError(t, os.Chmod(dir, 0o500))
	require.Error(t, s.Prune(6000))
	require.NoError(t, os.Chmod(dir, 0o700))

	require.NoError(t, s.Close())
	assert.Empty(t, chunkFiles(t, dir))
}

// timeAt maps a frame position back to its capture time, the inverse of
// FrameAt, so tests can check that mapping both ways.
func timeAt(s *Spool, frame int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.anchors
	switch {
	case len(a) == 0:
		return time.Time{}
	case len(a) == 1 || frame <= a[0].frame:
		return a[0].at.Add(s.format.Duration(frame - a[0].frame))
	}
	last := a[len(a)-1]
	if frame >= last.frame {
		return last.at.Add(s.format.Duration(frame - last.frame))
	}
	i := sort.Search(len(a), func(i int) bool { return a[i].frame > frame })
	lo, hi := a[i-1], a[i]
	frac := float64(frame-lo.frame) / float64(hi.frame-lo.frame)
	return lo.at.Add(time.Duration(frac * float64(hi.at.Sub(lo.at))))
}
