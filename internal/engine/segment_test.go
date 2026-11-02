package engine

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const simRate = 44100

var simT0 = time.Unix(1_700_000_000, 0) //nolint:gochecknoglobals // fixed test epoch

// linClock is a capture that started at t0 and never dropped a frame.
type linClock struct{ t0 time.Time }

func (c linClock) FrameAt(t time.Time) int64 {
	return int64(math.Round(t.Sub(c.t0).Seconds() * simRate))
}

func at(sec float64) time.Time { return simT0.Add(time.Duration(sec * float64(time.Second))) }

func frames(sec float64) int64 { return int64(math.Round(sec * simRate)) }

// song simulates Spotify playing one track. The song's audio position 0 is
// captured at audioStart (seconds after capture start); Spotify reports
// positions latency seconds ahead of the captured audio.
type song struct {
	dur        float64
	audioStart float64
	latency    float64
	poll       float64
	jitter     func(i int) float64
	// pauses: song position → pause length (seconds).
	pauseAt, pauseFor float64
	// rateSkew > 0 makes the reported position run faster than audio.
	rateSkew float64
	// granularity > 0 quantizes the reported position (held values).
	granularity float64
}

// timeline renders observations from the first poll after the song starts
// until it ends at playedTo (song seconds), plus the surrounding markers.
func (s song) timeline(playedTo float64) Timeline {
	poll := s.poll
	if poll == 0 {
		poll = 0.15
	}
	tl := Timeline{Duration: time.Duration(s.dur * float64(time.Second))}
	tl.Before = at(s.audioStart - s.latency - poll/2) // last obs of the previous track
	start := s.audioStart - s.latency                 // wall time Spotify reports position 0
	pausedFor := 0.0
	for i := 0; ; i++ {
		w := start + poll/2 + float64(i)*poll
		if s.jitter != nil {
			w += s.jitter(i)
		}
		pos := w - start - pausedFor
		if s.pauseFor > 0 && pos >= s.pauseAt && pausedFor == 0 {
			// Paused: emit paused observations for the pause length.
			pauseWall := start + s.pauseAt
			for p := pauseWall + poll/2; p < pauseWall+s.pauseFor; p += poll {
				tl.Obs = append(tl.Obs, Obs{At: at(p), Playing: false, Position: s.pauseAt})
			}
			pausedFor = s.pauseFor
			continue
		}
		if s.rateSkew != 0 {
			pos *= 1 + s.rateSkew
		}
		if pos > playedTo {
			tl.EndAt = at(w)
			return tl
		}
		reported := pos
		if s.granularity > 0 {
			reported = math.Floor(pos/s.granularity) * s.granularity
		}
		tl.Obs = append(tl.Obs, Obs{At: at(w), Playing: true, Position: reported})
	}
}

func segmentize(t *testing.T, tl Timeline, latency float64) Segment {
	t.Helper()
	tn := DefaultTuning()
	tn.Latency = time.Duration(latency * float64(time.Second))
	return Segmentize(tl, linClock{t0: simT0}, simRate, 0, tn)
}

func assertNear(t *testing.T, want, got int64, tolSec float64, what string) {
	t.Helper()
	assert.InDelta(t, float64(want), float64(got), tolSec*simRate, "%s: want %d got %d (Δ %.3fs)",
		what, want, got, float64(got-want)/simRate)
}

func TestSegmentCleanSong(t *testing.T) {
	t.Parallel()
	s := song{dur: 30, audioStart: 5}
	seg := segmentize(t, s.timeline(30), 0)

	require.True(t, seg.Complete, seg.Reason)
	require.Len(t, seg.Ranges, 1)
	margin := DefaultTuning().Margin.Seconds()
	assertNear(t, frames(5-margin), seg.Ranges[0].From, 0.001, "start")
	assertNear(t, frames(35+margin), seg.Ranges[0].To, 0.001, "end")
	assertNear(t, frames(5), seg.Anchor, 0.001, "anchor")
	assert.Equal(t, 1, seg.Runs)
	assert.Equal(t, seg.Ranges[0].To, seg.End())
	assert.Equal(t, seg.Ranges[0].Len(), seg.Frames())
}

func TestSegmentLatencyIsCompensated(t *testing.T) {
	t.Parallel()
	s := song{dur: 20, audioStart: 3, latency: 0.25}
	seg := segmentize(t, s.timeline(20), 0.25)
	require.True(t, seg.Complete, seg.Reason)
	assertNear(t, frames(3), seg.Anchor, 0.001, "anchor with latency")

	uncorrected := segmentize(t, s.timeline(20), 0)
	assertNear(t, frames(3-0.25), uncorrected.Anchor, 0.001, "without the correction the cut lands early")
}

func TestSegmentPollJitter(t *testing.T) {
	t.Parallel()
	// Up to ±40 ms of timestamp noise per poll, as osascript spawns add.
	jitter := func(i int) float64 { return 0.04 * math.Sin(float64(i)*1.7) }
	s := song{dur: 60, audioStart: 2, jitter: jitter}
	seg := segmentize(t, s.timeline(60), 0)
	require.True(t, seg.Complete, seg.Reason)
	assertNear(t, frames(2), seg.Anchor, 0.01, "median rejects jitter")
	assert.Less(t, seg.Drift, 50*time.Millisecond)
}

func TestSegmentHeldPositions(t *testing.T) {
	t.Parallel()
	// Spotify updating its position only every 0.5 s.
	s := song{dur: 40, audioStart: 2, granularity: 0.5}
	seg := segmentize(t, s.timeline(40), 0)
	require.True(t, seg.Complete, seg.Reason)
	assertNear(t, frames(2), seg.Anchor, 0.16, "first sample after each update is within a poll")
}

func TestSegmentPauseIsExcised(t *testing.T) {
	t.Parallel()
	s := song{dur: 20, audioStart: 1, pauseAt: 8, pauseFor: 3}
	seg := segmentize(t, s.timeline(20), 0)

	require.True(t, seg.Complete, seg.Reason)
	require.Len(t, seg.Ranges, 2)
	margin := DefaultTuning().Margin.Seconds()
	assertNear(t, frames(1-margin), seg.Ranges[0].From, 0.001, "first run start")
	assertNear(t, frames(1+8), seg.Ranges[0].To, 0.16, "cut at the pause")
	assertNear(t, frames(1+8+3), seg.Ranges[1].From, 0.16, "resume after the pause")
	assertNear(t, frames(1+20+3+margin), seg.Ranges[1].To, 0.001, "end")
	assertNear(t, frames(20+2*margin), seg.Frames(), 0.01, "the pause adds nothing")
	assert.Equal(t, 2, seg.Runs)
}

func TestSegmentStalePositionsAfterChange(t *testing.T) {
	t.Parallel()
	s := song{dur: 15, audioStart: 4}
	tl := s.timeline(15)
	// Spotify reports the previous track's position for two polls.
	tl.Obs[0].Position, tl.Obs[1].Position = 211.4, 211.6
	seg := segmentize(t, tl, 0)
	require.True(t, seg.Complete, seg.Reason)
	assertNear(t, frames(4), seg.Anchor, 0.001, "stale samples are dropped")
}

func TestSegmentNaturalEndIntoPause(t *testing.T) {
	t.Parallel()
	s := song{dur: 12, audioStart: 1}
	tl := s.timeline(12)
	last := tl.EndAt
	tl.EndAt = time.Time{}
	// The context ended: Spotify parks on the track, paused at 0:00.
	tl.Obs = append(tl.Obs, Obs{At: last, Playing: false, Position: 0})
	tl.EndAt = last.Add(time.Second)
	seg := segmentize(t, tl, 0)
	require.True(t, seg.Complete, seg.Reason)
	assertNear(t, frames(13+DefaultTuning().Margin.Seconds()), seg.End(), 0.001, "end")
}

func TestSegmentIncomplete(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		tl     func() Timeline
		reason string
	}{
		"joined late": {
			tl: func() Timeline {
				tl := song{dur: 30, audioStart: 5}.timeline(30)
				tl.Obs = tl.Obs[100:] // the session started 15 s into the song
				tl.Before = time.Time{}
				return tl
			},
			reason: ReasonJoinedLate,
		},
		"skipped before the end": {
			tl:     func() Timeline { return song{dur: 30, audioStart: 5}.timeline(18) },
			reason: ReasonEndedEarly,
		},
		"we moved on near the end": {
			tl: func() Timeline {
				tl := song{dur: 30, audioStart: 5}.timeline(29.6)
				tl.EndedByUs = true
				return tl
			},
			reason: ReasonEndedEarly,
		},
		"seek forward": {
			tl: func() Timeline {
				tl := song{dur: 30, audioStart: 5}.timeline(30)
				for i := 60; i < len(tl.Obs); i++ {
					tl.Obs[i].Position += 5
				}
				return tl
			},
			reason: ReasonSeekForward,
		},
		"seek back": {
			tl: func() Timeline {
				tl := song{dur: 30, audioStart: 5}.timeline(30)
				for i := 60; i < len(tl.Obs); i++ {
					tl.Obs[i].Position -= 5
				}
				return tl
			},
			reason: ReasonSeekBack,
		},
		"drift": {
			tl:     func() Timeline { return song{dur: 60, audioStart: 2, rateSkew: 0.025}.timeline(60) },
			reason: ReasonDrift,
		},
		"start not captured": {
			tl:     func() Timeline { return song{dur: 30, audioStart: 0.05}.timeline(30) },
			reason: ReasonStartLost,
		},
		"never played": {
			tl: func() Timeline {
				return Timeline{Duration: 30 * time.Second, Obs: []Obs{{At: at(1), Position: 0}}}
			},
			reason: ReasonNoAudio,
		},
		"duration mismatch": {
			tl: func() Timeline {
				tl := song{dur: 30, audioStart: 5}.timeline(30)
				tl.Duration = 300 * time.Second
				return tl
			},
			reason: ReasonEndedEarly,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			seg := segmentize(t, tc.tl(), 0)
			assert.False(t, seg.Complete)
			assert.Equal(t, tc.reason, seg.Reason)
		})
	}
}

func TestSegmentStopWhileStillPlaying(t *testing.T) {
	t.Parallel()
	tl := song{dur: 30, audioStart: 5}.timeline(30)
	tl.Obs = tl.Obs[:50]
	tl.EndAt = time.Time{} // still current: the session is stopping
	seg := segmentize(t, tl, 0)
	assert.False(t, seg.Complete)
	assert.Equal(t, ReasonEndedEarly, seg.Reason)
	require.Len(t, seg.Ranges, 1, "a partial still has its ranges for --keep-partial")
}

func TestMedian(t *testing.T) {
	t.Parallel()
	assert.InDelta(t, 0.0, median(nil), 0)
	assert.InDelta(t, 2.0, median([]float64{3, 1, 2}), 0)
	assert.InDelta(t, 2.5, median([]float64{4, 1, 3, 2}), 0)
}
