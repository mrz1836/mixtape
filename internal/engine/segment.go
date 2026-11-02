package engine

import (
	"math"
	"slices"
	"time"
)

// Tuning holds the timing constants of the segmenter. The defaults are
// derived from Spotify timing traces (docs/ARCHITECTURE.md explains each).
type Tuning struct {
	// Latency is how far Spotify's reported position runs ahead of the
	// audio that reaches the capture (decode → output pipeline).
	Latency time.Duration
	// Margin is added before the first and after the last audio of every
	// song, so estimation error never clips an edge.
	Margin time.Duration
	// StaleSlack is how far a new track's first positions may exceed the
	// time elapsed since the previous track was last seen before they are
	// rejected as stale (Spotify briefly reports the old track's position).
	StaleSlack time.Duration
	// SeekJump is the anchor jump that splits a run: a seek, or a stall.
	SeekJump time.Duration
	// Gap is the largest gap or overlap tolerated between runs.
	Gap time.Duration
	// StartSlack is how late the first run may start and still count as
	// "captured from the top".
	StartSlack time.Duration
	// EndSlack: a song ended naturally if its last position was within
	// EndSlack of its duration.
	EndSlack time.Duration
	// LengthUnder and LengthOver bound the audio length around the
	// reported duration.
	LengthUnder, LengthOver time.Duration
	// MaxDrift is the largest deviation of any sample from its run's
	// anchor: position and captured audio must advance together.
	MaxDrift time.Duration
}

// DefaultTuning returns the shipped segmenter constants.
func DefaultTuning() Tuning {
	return Tuning{
		// Measured with BlackHole and Spotify 1.2: audio reaches the
		// capture about 0.14 s after the position Spotify reports.
		Latency:     140 * time.Millisecond,
		Margin:      150 * time.Millisecond,
		StaleSlack:  300 * time.Millisecond,
		SeekJump:    750 * time.Millisecond,
		Gap:         500 * time.Millisecond,
		StartSlack:  500 * time.Millisecond,
		EndSlack:    1500 * time.Millisecond,
		LengthUnder: 1500 * time.Millisecond,
		LengthOver:  2 * time.Second,
		MaxDrift:    500 * time.Millisecond,
	}
}

// Obs is one observation of the player while a track was current.
type Obs struct {
	At       time.Time
	Playing  bool
	Position float64 // seconds; negative when unknown
}

// Timeline is everything the segmenter knows about one track.
type Timeline struct {
	Duration time.Duration
	// Before is the last observation of the previous track; zero when the
	// session started on this track.
	Before time.Time
	Obs    []Obs
	// EndAt is when the next track (or the end of the session) was
	// observed; zero while the track is still current.
	EndAt time.Time
	// EndedByUs is set when mixtape caused the transition (play, next,
	// stop), so it cannot count as a natural end.
	EndedByUs bool
}

// Clock maps capture time to a frame index (the spool implements it).
type Clock interface {
	FrameAt(t time.Time) int64
}

// Range is the half-open frame interval [From, To).
type Range struct {
	From, To int64
}

// Len is the number of frames in r.
func (r Range) Len() int64 { return max(r.To-r.From, 0) }

// Segment is the segmenter's verdict on one track.
type Segment struct {
	// Ranges are the frames to render, in order, with pauses excised and
	// the margins applied to the outer edges.
	Ranges []Range
	// Complete is true when the whole song was captured exactly once.
	Complete bool
	// Reason explains why the segment is incomplete.
	Reason string
	// Anchor is the frame where position 0 of the first run falls: the
	// estimated start of the song.
	Anchor int64
	// Runs is the number of uninterrupted play runs.
	Runs int
	// Drift is the worst deviation of a sample from its run's anchor.
	Drift time.Duration
	// Covered is the song time captured, without margins.
	Covered time.Duration
}

// End is the frame just after the last frame the segment needs.
func (s Segment) End() int64 {
	if len(s.Ranges) == 0 {
		return 0
	}
	return s.Ranges[len(s.Ranges)-1].To
}

// Frames is the total number of frames to render.
func (s Segment) Frames() int64 {
	var n int64
	for _, r := range s.Ranges {
		n += r.Len()
	}
	return n
}

// Incomplete reasons, stable strings that land in manifests and events.
const (
	ReasonNoAudio     = "nothing was captured while the song played"
	ReasonJoinedLate  = "recording started after the song began"
	ReasonStartLost   = "the start of the song is not in the capture"
	ReasonEndedEarly  = "the song did not play to its end"
	ReasonSeekForward = "playback skipped forward inside the song"
	ReasonSeekBack    = "playback jumped back inside the song"
	ReasonDrift       = "playback position and captured audio drifted apart"
	ReasonLength      = "the captured length does not match the song's duration"
)

// sample is one accepted playing observation.
type sample struct {
	at     time.Time
	pos    float64
	anchor float64 // frame of position 0, as this sample estimates it
}

// run is an uninterrupted stretch of playback.
type run struct {
	samples []sample
	before  time.Time // the observation preceding the first sample
	// pauseAt is the position reported while paused right after this run;
	// negative when the run did not end in a pause.
	pauseAt float64
	seekEnd bool // the run ended because the anchor jumped
	natural bool // the song played out and Spotify stopped on it
}

func (r *run) anchor() float64 {
	a := make([]float64, len(r.samples))
	for i, s := range r.samples {
		a[i] = s.anchor
	}
	return median(a)
}

// Segmentize turns a track's observations into the frame ranges to keep
// and decides whether the song was captured completely. rate is the
// capture rate; first is the oldest frame the capture holds.
//
// Each playing observation (time t, position p) estimates the frame where
// the run's position 0 lies: frame(t) + Latency·rate − p·rate. The median
// over a run is robust to poll jitter. A pause or a seek starts a new run;
// the audio between runs is excised.
func Segmentize(tl Timeline, clock Clock, rate int, first int64, tn Tuning) Segment {
	f := float64(rate)
	dur := tl.Duration.Seconds()
	runs := splitRuns(tl, clock, f, dur, tn)
	if len(runs) == 0 {
		return Segment{Reason: ReasonNoAudio}
	}

	seg := Segment{Complete: true, Runs: len(runs)}
	fail := func(reason string) {
		if seg.Complete {
			seg.Complete, seg.Reason = false, reason
		}
	}

	var prevEnd float64
	for i := range runs {
		r := &runs[i]
		a := r.anchor()
		start := runStart(runs, i)
		end := runEnd(tl, runs, i, dur, tn)
		if i == 0 {
			seg.Anchor = int64(math.Round(a))
			if start > tn.StartSlack.Seconds() {
				fail(ReasonJoinedLate)
			} else {
				start = 0 // a run that began within StartSlack began at the top
			}
		} else {
			switch gap := start - prevEnd; {
			case gap > tn.Gap.Seconds():
				fail(ReasonSeekForward)
			case gap < -tn.Gap.Seconds():
				fail(ReasonSeekBack)
			}
		}
		for _, s := range r.samples {
			if d := time.Duration(math.Abs(s.anchor-a) / f * float64(time.Second)); d > seg.Drift {
				seg.Drift = d
			}
		}
		seg.Ranges = append(seg.Ranges, Range{
			From: int64(math.Round(a + start*f)),
			To:   int64(math.Round(a + end*f)),
		})
		seg.Covered += time.Duration((end - start) * float64(time.Second))
		prevEnd = end
	}

	if seg.Drift > tn.MaxDrift {
		fail(ReasonDrift)
	}
	if prevEnd < dur-tn.EndSlack.Seconds() || (tl.EndedByUs && !runs[len(runs)-1].natural) {
		fail(ReasonEndedEarly)
	}
	if c := seg.Covered; c < tl.Duration-tn.LengthUnder || c > tl.Duration+tn.LengthOver {
		fail(ReasonLength)
	}

	margin := int64(tn.Margin.Seconds() * f)
	seg.Ranges[0].From -= margin
	seg.Ranges[len(seg.Ranges)-1].To += margin
	if seg.Ranges[0].From < first {
		fail(ReasonStartLost)
		seg.Ranges[0].From = min(first, seg.Ranges[0].To)
	}
	return seg
}

// splitRuns groups accepted playing observations into runs.
func splitRuns(tl Timeline, clock Clock, f, dur float64, tn Tuning) []run {
	var (
		runs     []run
		cur      *run
		prevAt   = tl.Before
		accepted bool
	)
	closeRun := func() {
		if cur != nil && len(cur.samples) > 0 {
			runs = append(runs, *cur)
		}
		cur = nil
	}
	for _, o := range tl.Obs {
		if !o.Playing || o.Position < 0 {
			if cur != nil && !o.Playing && o.Position >= 0 {
				last := cur.samples[len(cur.samples)-1].pos
				if last >= dur-tn.EndSlack.Seconds() && o.Position < last-tn.Gap.Seconds() {
					cur.natural = true // played out; Spotify parked on the track at 0:00
				} else {
					cur.pauseAt = o.Position
				}
			}
			closeRun()
			prevAt = o.At
			continue
		}
		if o.Position == 0 {
			prevAt = o.At
			continue // Spotify reports exactly 0 while it buffers the start
		}
		if !accepted && !tl.Before.IsZero() &&
			o.Position > o.At.Sub(tl.Before).Seconds()+tn.StaleSlack.Seconds() {
			prevAt = o.At
			continue // the previous track's position, reported under the new ID
		}
		if cur != nil && o.Position == cur.samples[len(cur.samples)-1].pos {
			prevAt = o.At
			continue // a held value carries no new timing information
		}
		accepted = true
		s := sample{
			at:     o.At,
			pos:    o.Position,
			anchor: float64(clock.FrameAt(o.At)) + tn.Latency.Seconds()*f - o.Position*f,
		}
		if cur != nil && math.Abs(s.anchor-recentAnchor(cur)) > tn.SeekJump.Seconds()*f {
			cur.seekEnd = true
			closeRun()
		}
		if cur == nil {
			cur = &run{before: prevAt, pauseAt: -1}
		}
		cur.samples = append(cur.samples, s)
		prevAt = o.At
	}
	closeRun()
	return runs
}

// recentAnchor is the median anchor of a run's last few samples, the
// reference a new sample is compared with to detect a seek.
func recentAnchor(r *run) float64 {
	n := len(r.samples)
	from := max(n-5, 0)
	a := make([]float64, 0, n-from)
	for _, s := range r.samples[from:] {
		a = append(a, s.anchor)
	}
	return median(a)
}

// runStart is the song position where run i began.
func runStart(runs []run, i int) float64 {
	r := runs[i]
	s0 := r.samples[0]
	if i > 0 && runs[i-1].pauseAt >= 0 && !runs[i-1].seekEnd {
		// Resumed after a pause: playback continues from the paused position.
		return runs[i-1].pauseAt
	}
	start := s0.pos
	if !r.before.IsZero() {
		start -= s0.at.Sub(r.before).Seconds()
	}
	if i > 0 && runs[i-1].seekEnd {
		start = s0.pos - s0.at.Sub(runs[i-1].samples[len(runs[i-1].samples)-1].at).Seconds()
	}
	return max(start, 0)
}

// runEnd is the song position where run i ended.
func runEnd(tl Timeline, runs []run, i int, dur float64, tn Tuning) float64 {
	r := runs[i]
	last := r.samples[len(r.samples)-1]
	if r.natural {
		return dur
	}
	if r.pauseAt >= 0 {
		return r.pauseAt
	}
	if r.seekEnd {
		return last.pos
	}
	if i < len(runs)-1 || tl.EndAt.IsZero() {
		return last.pos
	}
	if !tl.EndedByUs && last.pos >= dur-tn.EndSlack.Seconds() {
		return dur // played out to the natural end
	}
	return min(last.pos+tl.EndAt.Sub(last.at).Seconds(), dur)
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := slices.Clone(v)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}
