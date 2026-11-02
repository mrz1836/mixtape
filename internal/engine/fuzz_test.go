package engine

import (
	"encoding/binary"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fuzzTimeline decodes a fuzz script into a track's observations. Each
// 4-byte step is: the wait since the last observation (0–510 ms), whether
// Spotify was playing, and the reported position (−327.68 s to 327.67 s;
// negative means unknown). Time only moves forward, as on the real clock.
func fuzzTimeline(script []byte, dur time.Duration, before, ended, endedByUs bool) Timeline {
	tl := Timeline{Duration: dur, EndedByUs: endedByUs}
	now := simT0.Add(time.Second)
	if before {
		tl.Before = simT0
	}
	for i := 0; i+4 <= len(script); i += 4 {
		now = now.Add(time.Duration(script[i]) * 2 * time.Millisecond)
		pos := float64(int16(binary.BigEndian.Uint16(script[i+2:]))) / 100
		tl.Obs = append(tl.Obs, Obs{At: now, Playing: script[i+1]&1 == 1, Position: pos})
	}
	if ended {
		tl.EndAt = now.Add(150 * time.Millisecond)
	}
	return tl
}

// FuzzSegmentize checks the segmenter's contract on any observation
// sequence: it never panics, its ranges are well formed (and in order for
// a complete song), and its verdict is consistent.
func FuzzSegmentize(f *testing.F) {
	clean := make([]byte, 0, 4*200)
	for i := range 200 { // a 30 s song polled every 150 ms
		clean = append(clean, 75, 1)
		clean = binary.BigEndian.AppendUint16(clean, uint16(i*15))
	}
	f.Add(clean, uint32(30000), true, true, false, int64(0))
	f.Add(clean[:400], uint32(30000), true, true, true, int64(0))      // cut short by mixtape
	f.Add(clean, uint32(30000), false, false, false, int64(1_000_000)) // the start pruned away
	f.Add([]byte{75, 0, 0, 0, 75, 1, 0xff, 0xff}, uint32(1000), true, true, false, int64(0))
	f.Add([]byte{}, uint32(0), false, false, false, int64(0))
	// A seek past the duration once made a run end before it started.
	f.Add([]byte("01\x0000100"), uint32(30000), true, true, true, int64(0))
	// A seek while paused was once taken for a resume in place, and the
	// song for complete.
	f.Add([]byte("00000000000021\x00000\x00"+strings.Repeat("0", 85)+"0100"), uint32(30000), false, true, false, int64(3))

	f.Fuzz(func(t *testing.T, script []byte, durMS uint32, before, ended, endedByUs bool, first int64) {
		dur := time.Duration(durMS%(20*60*1000)) * time.Millisecond // up to 20 minutes
		tl := fuzzTimeline(script, dur, before, ended, endedByUs)
		seg := Segmentize(tl, linClock{t0: simT0}, simRate, first, DefaultTuning())

		var total int64
		for i, r := range seg.Ranges {
			require.LessOrEqual(t, r.From, r.To, "range %d is backwards: %+v", i, seg.Ranges)
			// A kept song is rendered range by range: they must follow
			// the capture. (Positions that contradict the clock can
			// disorder the ranges of a song that is discarded anyway.)
			if i > 0 && seg.Complete {
				require.LessOrEqual(t, seg.Ranges[i-1].From, r.From, "ranges out of order: %+v", seg.Ranges)
			}
			total += r.Len()
		}
		assert.Equal(t, total, seg.Frames())
		assert.GreaterOrEqual(t, seg.Runs, 0)
		assert.GreaterOrEqual(t, seg.Drift, time.Duration(0))
		if seg.Complete {
			assert.Empty(t, seg.Reason, "a complete song has no reason")
			assert.NotEmpty(t, seg.Ranges, "a complete song has audio")
		} else {
			assert.NotEmpty(t, seg.Reason, "an incomplete song says why")
		}
		if len(seg.Ranges) > 0 {
			assert.Equal(t, seg.Ranges[len(seg.Ranges)-1].To, seg.End())
		}
	})
}
