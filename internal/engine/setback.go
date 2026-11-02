package engine

import (
	"fmt"
	"time"

	"github.com/mrz1836/mixtape/internal/wav"
)

// setbackKind classifies a song that could not be recorded.
type setbackKind int

const (
	setbackOther      setbackKind = iota // cut short, drifted, failed to save
	setbackSilent                        // played, but no sound reached mixtape
	setbackNotStarted                    // Spotify did not start it
)

// setback counts a song (or one attempt at it) that could not be recorded
// for a reason outside mixtape's and the listener's control. After
// MaxFailures in a row the session ends with an error naming the likely
// cause, instead of trying every song the same way: signed out, offline,
// playing on another device, or muted. A recorded song resets the count.
func (e *Engine) setback(kind setbackKind, reason string) {
	if e.opts.MaxFailures <= 0 || e.plan.Mode == ModeLive || e.stopping {
		return
	}
	e.streak = append(e.streak, kind)
	e.lastSetback = reason
	if len(e.streak) >= e.opts.MaxFailures {
		e.fail(e.streakError())
	}
}

// streakError names what the setbacks in a row have in common.
func (e *Engine) streakError() error {
	n := len(e.streak)
	all := func(k setbackKind) bool {
		for _, s := range e.streak {
			if s != k {
				return false
			}
		}
		return true
	}
	switch {
	case all(setbackSilent):
		return fmt.Errorf("%w (%d in a row)", ErrNoSound, n)
	case all(setbackNotStarted):
		return fmt.Errorf("%w (%d in a row)", ErrNotStarting, n)
	}
	return fmt.Errorf("%w (%d in a row; the last: %s)", ErrFailing, n, e.lastSetback)
}

// tooLong explains why a song of duration d cannot be recorded (its WAV
// would pass the format's 4 GiB limit), or returns "".
func (e *Engine) tooLong(d time.Duration) string {
	if e.cfg.Source == nil || d <= 0 {
		return ""
	}
	f := e.cfg.Source.Format()
	if limit := wav.MaxDuration(f.Rate, f.Channels); limit > 0 && d > limit {
		return fmt.Sprintf("too long to record: %s, and a recording can hold at most %s at %d Hz",
			d.Round(time.Minute), limit.Round(time.Minute), f.Rate)
	}
	return ""
}
