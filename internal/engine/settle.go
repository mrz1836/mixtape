package engine

import (
	"fmt"
	"math"

	"github.com/mrz1836/mixtape/internal/library"
)

// finalizeReady settles every ended take whose audio is fully spooled
// (or has waited long enough). force settles them all now.
func (e *Engine) finalizeReady(force bool) {
	if len(e.finals) == 0 {
		return
	}
	// Detach the list first: settling can end the session, which closes
	// the current take and re-enters here with it.
	pending := e.finals
	e.finals = nil
	now := e.now()
	for _, t := range pending {
		seg := e.segment(t)
		if t.waitedAt.IsZero() {
			t.waitedAt = now
		}
		if !force && seg.End() > e.spoolFrames() && now.Sub(t.waitedAt) < audioWait {
			e.finals = append(e.finals, t)
			continue
		}
		e.settle(t, seg)
	}
}

// forceFinalize settles every pending take immediately.
func (e *Engine) forceFinalize() {
	if e.cur != nil {
		e.endTake(e.now(), true)
	}
	e.finalizeReady(true)
}

// segment runs the segmenter over a take.
func (e *Engine) segment(t *take) Segment {
	tl := Timeline{
		Duration: t.track.Duration, Before: t.before, Obs: t.obs,
		EndAt: t.endAt, EndedByUs: t.endedByUs,
	}
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool == nil {
		return Segment{Reason: ReasonNoAudio}
	}
	return Segmentize(tl, e.spool, e.spool.Format().Rate, e.spool.Oldest(), e.tn)
}

// settle decides a finished take: save it, keep it as a partial, or
// discard it (and retry or fail the song).
func (e *Engine) settle(t *take, seg Segment) {
	it := t.item
	if e.falseStart(t, seg) {
		if e.opts.Verbose {
			e.emitBoundary(t, seg)
		}
		return
	}
	e.announce(t) // whatever becomes of the take follows its start
	it.attempts++
	reason := t.invalid
	if reason == "" {
		reason = seg.Reason
	}
	complete := seg.Complete && t.invalid == ""
	if e.opts.Verbose {
		e.emitBoundary(t, seg)
	}
	if e.crossfaded(t, seg) {
		return
	}

	switch {
	case complete || (e.opts.KeepPartial && seg.Frames() > 0 && !t.userSkipped):
		path, err := e.render(t, seg)
		if err != nil {
			it.state = itemPending
			e.failItem(it, "render: "+err.Error())
			e.setback(setbackOther, "render: "+err.Error())
			return
		}
		e.submitCommit(it, t, path, !complete, seg.Covered)
		if !complete {
			it.state = itemPending // a complete take will replace the partial
			e.discardOrRetry(t, reason, true)
		}
	default:
		it.state = itemPending
		e.discardOrRetry(t, reason, false)
	}
}

// maxFalseStarts bounds how often a song may start falsely before its
// takes count as attempts.
const maxFalseStarts = 3

// falseStart recognizes a take of a song that never really began: Spotify
// moved on (to the track it resumed, say) before the song believably played
// earlyRestart seconds. That is no attempt and nothing to report; the song
// is simply requested again. A take the session's end cut short is
// discarded as usual.
func (e *Engine) falseStart(t *take, seg Segment) bool {
	it := t.item
	if seg.Complete || t.userSkipped || t.invalid != "" || e.stopping || e.plan.Mode == ModeLive ||
		e.started(t) || it.falseStarts >= maxFalseStarts {
		return false
	}
	it.falseStarts++
	it.state = itemPending
	return true
}

// crossfaded recognizes Spotify's crossfade: consecutive songs that change
// on their own the same few seconds before their end. It ends the session
// rather than discard every song.
func (e *Engine) crossfaded(t *take, seg Segment) bool {
	gap := t.track.Duration.Seconds() - t.lastPos()
	if seg.Complete || seg.Reason != ReasonEndedEarly || t.endedByUs || t.userSkipped ||
		gap < e.tn.EndSlack.Seconds() || gap > crossfadeMax.Seconds() {
		e.earlyEnds = e.earlyEnds[:0]
		return false
	}
	e.earlyEnds = append(e.earlyEnds, gap)
	n := len(e.earlyEnds)
	if n < 2 || math.Abs(e.earlyEnds[n-1]-e.earlyEnds[n-2]) > e.tn.EndSlack.Seconds() {
		return false
	}
	t.item.state = itemPending
	e.counts.Discarded++
	e.emit(Event{Type: EventTrackDiscarded, Track: t.eventTrack(), Reason: ReasonEndedEarly})
	e.fail(fmt.Errorf("%w (they change %.1fs before the end)", ErrCrossfade, gap))
	return true
}

// discardOrRetry handles an incomplete take: the song is retried once
// unless the user skipped it; kept reports a saved partial.
func (e *Engine) discardOrRetry(t *take, reason string, kept bool) {
	it := t.item
	if !kept {
		e.counts.Discarded++
		e.emit(Event{Type: EventTrackDiscarded, Track: t.eventTrack(), Reason: reason})
	}
	if !t.userSkipped && !t.endedByUs {
		kind := setbackOther
		if t.silent {
			kind = setbackSilent
		}
		e.setback(kind, reason)
	}
	switch {
	case t.userSkipped:
		e.skip(it, t.eventTrack(), "skipped by you")
	case e.plan.Mode == ModeLive:
		it.state = itemDone // live mode records what plays; nothing to retry
	case it.attempts >= maxTakes:
		e.failItem(it, reason)
	}
}

// failItem gives up on a song for this session and records it failed.
func (e *Engine) failItem(it *Item, reason string) {
	if it.state == itemDone || it.state == itemFailed {
		return
	}
	e.submit(job{kind: jobMark, item: it, status: library.StatusFailed, reason: reason})
	e.markFailed(it, reason)
}

// markFailed counts and reports a song given up on. The caller records the
// status in the library (or the library already has).
func (e *Engine) markFailed(it *Item, reason string) {
	it.state, it.reason = itemFailed, reason
	e.counts.Failed++
	e.emit(Event{Type: EventTrackFailed, Track: itemTrack(it), Reason: reason})
}

// reasonInLibrary is why a song already recorded is skipped.
const reasonInLibrary = "already in the library"

// skip counts and reports a song that needs nothing more this session.
func (e *Engine) skip(it *Item, track *EventTrack, reason string) {
	it.state = itemDone
	e.counts.Skipped++
	e.emit(Event{Type: EventTrackSkipped, Track: track, Reason: reason})
}

// emitBoundary reports the measured timing of a song (--verbose).
func (e *Engine) emitBoundary(t *take, seg Segment) {
	var rate int
	var observed int64 // the frame at the first observation of the track
	e.spoolMu.Lock()
	if e.spool != nil {
		rate = e.spool.Format().Rate
		observed = e.spool.FrameAt(t.startedAt)
	}
	e.spoolMu.Unlock()
	if rate == 0 {
		return
	}
	drift := seg.Drift.Seconds()
	ev := Event{Type: EventBoundary, Track: t.eventTrack(), Runs: seg.Runs, DriftSeconds: &drift, Reason: seg.Reason}
	if len(seg.Ranges) > 0 {
		// Seconds between the capture's first observation of the track and
		// the estimated start of its audio: latency plus poll delay.
		first := float64(seg.Anchor-observed) / float64(rate)
		ev.OffsetSeconds = &first
	}
	e.emit(ev)
}
