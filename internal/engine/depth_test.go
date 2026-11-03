package engine

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/wav"
)

// eachDepth runs fn as a parallel subtest at 16 and 24 bits a sample.
func eachDepth(t *testing.T, fn func(t *testing.T, bits int)) {
	t.Helper()
	for _, bits := range []int{16, 24} {
		t.Run(strconv.Itoa(bits)+"-bit", func(t *testing.T) {
			t.Parallel()
			fn(t, bits)
		})
	}
}

// newSimAt is newSim with a capture at bits bits a sample.
func newSimAt(t *testing.T, bits int, tracks ...simTrack) *sim {
	t.Helper()
	s := newSim(t, tracks...)
	s.source.bits = bits
	return s
}

// requireWAVsAt checks that every WAV the encoder was handed is at bits.
func requireWAVsAt(t *testing.T, s *sim, bits int) {
	t.Helper()
	calls := s.enc.Calls()
	require.NotEmpty(t, calls)
	for _, c := range calls {
		assert.Equal(t, bits, c.Bits, "WAV %s", c.WAV)
	}
}

// The sessions that matter most behave alike at 16 and 24 bits: songs are
// cut to the same frames, pauses are excised, the keys work, and silence
// is still silence.
func TestKeySessionsAtEveryDepth(t *testing.T) {
	t.Parallel()
	margins := 2 * framesOf(DefaultTuning().Margin)
	tolerance := float64(framesOf(pollDur))

	t.Run("a queue of singles", func(t *testing.T) {
		t.Parallel()
		eachDepth(t, func(t *testing.T, bits int) {
			s := newSimAt(t, bits, songA, songB)
			_, res := s.run(singles(songA, songB), Options{}, nil)
			requireComplete(t, res)
			assert.Equal(t, 2, res.Counts.Recorded)
			for _, n := range s.enc.totalFrames() {
				want := framesOf(songA.dur) + margins
				if other := framesOf(songB.dur) + margins; abs64(n-other) < abs64(n-want) {
					want = other
				}
				assert.InDelta(t, want, n, tolerance, "the cut of %d frames is one song plus margins", n)
			}
			requireWAVsAt(t, s, bits)
		})
	})
	t.Run("a pause in Spotify is cut out", func(t *testing.T) {
		t.Parallel()
		eachDepth(t, func(t *testing.T, bits int) {
			s := newSimAt(t, bits, songC)
			s.at[2*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = false }) }
			s.at[5*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = true }) }
			_, res := s.run(singles(songC), Options{}, nil)
			requireComplete(t, res)
			frames := s.enc.totalFrames()
			require.Len(t, frames, 1)
			assert.InDelta(t, framesOf(songC.dur)+margins, frames[0], tolerance)
		})
	})
	t.Run("the keys: pause twice, then skip", func(t *testing.T) {
		t.Parallel()
		eachDepth(t, func(t *testing.T, bits int) {
			s := newSimAt(t, bits, songC, songA)
			controls := make(chan Control, 3)
			s.at[2*time.Second] = func(*sim) { controls <- ControlPlayPause }
			s.at[4*time.Second] = func(*sim) { controls <- ControlPlayPause }
			pl := s.playlist("pl0000000000000000000k", "Keys", true, songC, songA)
			s.at[11*time.Second] = func(*sim) { controls <- ControlNext }
			_, res := s.run([]spotify.Collection{pl}, Options{MaxFailures: 3}, controls)
			requireComplete(t, res)
			assert.Equal(t, 1, res.Counts.Recorded, "the paused song is recorded whole")
			assert.Equal(t, 1, res.Counts.Skipped, "the skipped one is not")
			assert.Equal(t, 2, strings.Count(strings.Join(s.spotify.Commands(), ","), "playpause"))
			frames := s.enc.totalFrames()
			require.Len(t, frames, 1)
			assert.InDelta(t, framesOf(songC.dur)+margins, frames[0], tolerance)
		})
	})
	t.Run("silence is silence", func(t *testing.T) {
		t.Parallel()
		eachDepth(t, func(t *testing.T, bits int) {
			long := simTrack{id: "0000000000000000000lll", name: "Long", artist: "X", dur: 30 * time.Second}
			s := newSimAt(t, bits, long)
			s.source.silent = true
			_, res := s.run(singles(long), Options{}, nil)
			require.ErrorIs(t, res.Err, ErrSilentDevice)
		})
	})
}

// A 24-bit capture reaches the WAV whole: header, frame count, and the
// detail below the 16 bits a 16-bit capture would keep.
func TestTwentyFourBitCaptureKeepsItsLowBits(t *testing.T) {
	t.Parallel()
	s := newSimAt(t, 24, songA)
	_, res := s.run(singles(songA), Options{KeepWAV: true}, nil)
	requireComplete(t, res)
	calls := s.enc.Calls()
	require.Len(t, calls, 1)
	data, err := os.ReadFile(calls[0].WAV)
	require.NoError(t, err)
	assert.Equal(t, uint16(24), binary.LittleEndian.Uint16(data[34:36]))
	assert.Equal(t, uint16(6), binary.LittleEndian.Uint16(data[32:34]), "block align: stereo, three bytes a sample")
	pcm := data[wav.HeaderSize:]
	require.Zero(t, len(pcm)%6)
	v := uint32(loudLevel<<8 | 0x5A) // what the fake source writes at 24 bits
	loud := []byte{byte(v), byte(v >> 8), byte(v >> 16)}
	assert.True(t, bytes.Contains(pcm, loud), "the low byte the source captured is in the WAV")
	assert.Equal(t, loudLevel, capture.Peak(pcm, capture.Format{Rate: testRate, Channels: 2, Bits: 24}))
}

func TestWriteWAVHeaderFollowsTheSpool(t *testing.T) {
	t.Parallel()
	for _, f := range []capture.Format{
		{Rate: testRate, Channels: 2}, {Rate: testRate, Channels: 2, Bits: 16}, {Rate: 44100, Channels: 2, Bits: 24},
	} {
		src := &stubSpool{bpf: f.BytesPerFrame()}
		path := filepath.Join(t.TempDir(), "take.wav")
		require.NoError(t, writeWAV(path, src, f, []Range{{From: 0, To: 4}}))
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		le := binary.LittleEndian
		assert.Equal(t, uint16(f.SampleBits()), le.Uint16(data[34:36]), "%+v", f)
		assert.Equal(t, uint32(f.Rate), le.Uint32(data[24:28]), "%+v", f)
		assert.Equal(t, uint32(4*f.BytesPerFrame()), le.Uint32(data[40:44]), "%+v", f)
		assert.Len(t, data, wav.HeaderSize+4*f.BytesPerFrame())
	}
}
