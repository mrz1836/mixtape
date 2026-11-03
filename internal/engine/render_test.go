package engine

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/wav"
)

var errChunkGone = errors.New("chunk file vanished")

// stubSpool serves every frame as bytes of the range's number, so a WAV
// shows which range each byte came from. It can fail, or come up one
// frame short, on one range (1-based; 0: never).
type stubSpool struct {
	bpf     int
	failOn  int
	shortOn int
	calls   int
}

func (r *stubSpool) ReadRange(from, to int64, w io.Writer) (int64, error) {
	r.calls++
	n := to - from
	switch r.calls {
	case r.failOn:
		return 0, errChunkGone
	case r.shortOn:
		n--
	}
	_, err := w.Write(bytes.Repeat([]byte{byte(r.calls)}, int(n)*r.bpf))
	return n, err
}

func TestClipRangesKeepsOnlyWhatTheSpoolHolds(t *testing.T) {
	t.Parallel()
	ranges := []Range{{From: 0, To: 40}, {From: 30, To: 100}, {From: 150, To: 300}, {From: 400, To: 500}, {From: 600, To: 700}}
	assert.Equal(t, []Range{{From: 50, To: 100}, {From: 150, To: 300}, {From: 400, To: 450}}, clipRanges(ranges, 50, 450),
		"pruned audio and audio not yet spooled are left out")
	assert.Equal(t, Range{From: 30, To: 100}, ranges[1], "the segment's own ranges are not changed")
	assert.Empty(t, clipRanges(ranges, 800, 900), "everything already pruned")
	assert.Empty(t, clipRanges(nil, 0, 100))
}

func TestWriteWAVWritesEveryRangeAfterTheHeader(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: testRate, Channels: 2}
	src := &stubSpool{bpf: f.BytesPerFrame()}
	path := filepath.Join(t.TempDir(), "take.wav")

	require.NoError(t, writeWAV(path, src, f, []Range{{From: 0, To: 3}, {From: 10, To: 12}}))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, data, wav.HeaderSize+5*f.BytesPerFrame())
	assert.Equal(t, uint32(5*f.BytesPerFrame()), binary.LittleEndian.Uint32(data[wav.HeaderSize-4:]),
		"the header's data size counts every frame")
	assert.Equal(t, append(bytes.Repeat([]byte{1}, 12), bytes.Repeat([]byte{2}, 8)...), data[wav.HeaderSize:])
}

func TestWriteWAVErrors(t *testing.T) {
	t.Parallel()
	f := capture.Format{Rate: testRate, Channels: 2}
	ranges := []Range{{From: 0, To: 100}, {From: 200, To: 250}, {From: 300, To: 310}}
	for name, tc := range map[string]struct {
		src       *stubSpool
		dir       func(t *testing.T) string
		wantIs    error
		wantText  string
		wantCalls int
	}{
		"a read that fails stops the write": {
			src: &stubSpool{failOn: 2}, wantIs: errChunkGone, wantCalls: 2,
		},
		"a read that comes up short is an error": {
			src: &stubSpool{shortOn: 1}, wantIs: errShortRead, wantText: "99 of 100 frames", wantCalls: 1,
		},
		"a folder that is not there": {
			src: &stubSpool{},
			dir: func(t *testing.T) string {
				t.Helper()
				return filepath.Join(t.TempDir(), "missing")
			},
			wantIs: os.ErrNotExist, wantCalls: 0,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			tc.src.bpf = f.BytesPerFrame()
			dir := t.TempDir()
			if tc.dir != nil {
				dir = tc.dir(t)
			}
			err := writeWAV(filepath.Join(dir, "take.wav"), tc.src, f, ranges)
			require.ErrorIs(t, err, tc.wantIs)
			if tc.wantText != "" {
				require.ErrorContains(t, err, tc.wantText)
			}
			assert.Equal(t, tc.wantCalls, tc.src.calls)
		})
	}
}

func TestSafeNameKeepsOnlyLettersAndDigits(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "4uLU6hMCjMI75M1A2tKUQC", safeName("4uLU6hMCjMI75M1A2tKUQC"))
	assert.Equal(t, "spotify_track_x__1", safeName("spotify:track:x/.1"))
	assert.Equal(t, "_", safeName("é"), "one rune, one replacement")
}
