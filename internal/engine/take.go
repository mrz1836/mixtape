package engine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/wav"
)

// take is one stretch of Spotify playing one track: from the first
// observation of the track to the change to another (or a restart).
type take struct {
	key       string
	track     spotify.Track
	item      *Item // the planned song this take records; nil when not recording
	recording bool
	ad        bool
	passive   bool // live mode: filtered out by --match, left alone
	before    time.Time
	obs       []Obs
	startedAt time.Time
	endAt     time.Time
	endedByUs bool
	// userSkipped: the user pressed next; the song is not retried.
	userSkipped bool
	// sawPlaying: Spotify played this take at some point.
	sawPlaying bool
	// invalid holds the first capture or silence problem seen.
	invalid  string
	waitedAt time.Time // when the take first waited for its audio
	// midSince: when the song began playing from the middle, as Spotify
	// resuming it where it was last left (see checkResumed).
	midSince time.Time
	// announced: track_started was emitted for this take.
	announced bool
}

func (t *take) lastPos() float64 {
	for _, o := range slices.Backward(t.obs) {
		if o.Position >= 0 {
			return o.Position
		}
	}
	return -1
}

func (t *take) eventTrack() *EventTrack {
	et := &EventTrack{
		ID: t.track.ID, Title: t.track.Name, Artist: t.track.Artist,
		DurationMS: t.track.Duration.Milliseconds(),
	}
	if it := t.item; it != nil {
		et.ID, et.Collection, et.Position = it.Track.ID, it.Group.Spec.Name, it.position()
		if t.track.ID != "" && t.track.ID != it.Track.ID {
			et.PlayedID = t.track.ID
		}
		et.Attempt = it.attempts
	}
	return et
}

// blipWindow is how long another track must persist before a take that has
// not really started gives way to it.
const blipWindow = time.Second

// onSnapshot folds one player observation into the session.
//
// Another track for a moment is a blip, not a change: while it loads a song,
// Spotify can report the next queued track, or the previously played one at
// its old position, for a few polls. A change from a take that is clearly
// playing to a track that starts at its top is accepted on the next
// observation; any other change (the take has no believable position yet,
// or the other track appears mid-song) only once the other track persists
// for blipWindow. The held observations are then replayed, so the change
// keeps its first observation's time and the new take loses nothing.
func (e *Engine) onSnapshot(ctx context.Context, s spotify.Snapshot) {
	if s.Err != nil {
		e.onPlayerError(s)
		return
	}
	t := e.cur
	if t == nil || s.Track.URI == t.key {
		e.pending = nil
		e.observe(ctx, s)
		return
	}
	if len(e.pending) > 0 && e.pending[0].Track.URI != s.Track.URI {
		e.pending = e.pending[:0] // a different track again: start over
	}
	e.pending = append(e.pending, s)
	window := time.Duration(0)
	if !e.believable(t) || e.pending[0].Position >= blipWindow.Seconds() {
		window = blipWindow
	}
	if len(e.pending) < 2 || s.At.Sub(e.pending[0].At) < window {
		return
	}
	held := e.pending
	e.pending = nil
	for _, p := range held {
		e.observe(ctx, p)
	}
}

// observe applies one accepted observation.
func (e *Engine) observe(ctx context.Context, s spotify.Snapshot) {
	e.goneSince = time.Time{}
	if e.transitional(s) {
		return // half-updated mid-change: the next title under the old URI
	}
	e.last = s
	if e.cur != nil && s.Track.URI == e.cur.key {
		e.refineDuration(s)
		if e.restarted(s) && (e.cur.lastPos() < earlyRestart || !e.believable(e.cur)) {
			e.resetTake(s) // Spotify starting the song over right after it began
		}
	}
	if e.cur != nil && (s.Track.URI != e.cur.key || e.restarted(s) || e.parked(s)) {
		e.endTake(s.At, false)
	}
	if e.cur == nil && s.Track.URI != "" && !e.stopping {
		e.beginTake(ctx, s)
	}
	if t := e.cur; t != nil {
		playing := s.State == spotify.StatePlaying
		t.sawPlaying = t.sawPlaying || playing
		t.obs = append(t.obs, Obs{At: s.At, Playing: playing, Position: s.Position})
		if t.recording && playing {
			e.lastActive = s.At
		}
		e.promote(ctx, s)
		if e.cur == t {
			e.checkResumed(ctx, t, s)
		}
		if e.cur == t && t.recording && e.believable(t) {
			e.announce(t)
		}
	}
	e.prevAt = s.At
	e.finalizeReady(false)
	if !e.stopping {
		e.steer(ctx, s)
	}
	if s.At.Sub(e.lastTick) >= tickEvery {
		e.onTick(ctx) // housekeeping on the player's clock; the ticker covers silence
	}
}

// restarted reports a jump back to the top of the same track: repeat-one,
// the user (or mixtape) restarting the song, or Spotify itself starting a
// freshly loaded song over (seen right after "play track").
func (e *Engine) restarted(s spotify.Snapshot) bool {
	if s.State != spotify.StatePlaying || s.Position < 0 || s.Position > e.tn.StartSlack.Seconds() {
		return false
	}
	return e.cur.lastPos() > s.Position+restartJump
}

// restartJump is the backward jump that counts as a restart; Spotify's
// positions never move backwards during playback. A restart before
// earlyRestart seconds is a false start: Spotify routinely plays the first
// fraction of a second of a freshly loaded song, then starts it over.
const (
	restartJump  = 0.05
	earlyRestart = 2.0
)

// believable reports whether any playing observation of t has a position a
// song that began with the take could have reached. After "play track",
// Spotify can report the previous track's position under the new URI for a
// moment, then jump to 0; that jump is a false start, not a restart.
func (e *Engine) believable(t *take) bool { return e.reached(t) > 0 }

// reached is the furthest position t believably played: positions beyond
// what a song that began with the take could have reached (the previous
// track's, or one Spotify resumed from) do not count.
func (e *Engine) reached(t *take) float64 {
	best := 0.0
	for _, o := range t.obs {
		if !o.Playing || o.Position <= best {
			continue
		}
		if e.plausible(t, o.At, o.Position) {
			best = o.Position
		}
	}
	return best
}

// plausible reports whether a song that began with t could be at pos by at.
func (e *Engine) plausible(t *take, at time.Time, pos float64) bool {
	return t.before.IsZero() || pos <= at.Sub(t.before).Seconds()+e.tn.StaleSlack.Seconds()
}

// checkResumed restarts a requested song that Spotify resumed from the
// middle. Without a context to play it in, "play track" picks a song up
// where it was last left, so it plays from there on. A stale position
// lasts a moment; one that holds for blipWindow is a resume, and the song
// is started over from the top in a fresh take.
func (e *Engine) checkResumed(ctx context.Context, t *take, s spotify.Snapshot) {
	mid := e.plan.Mode != ModeLive && t.item != nil && t.item.state == itemPending && !t.ad && !e.stopping &&
		s.State == spotify.StatePlaying && s.Position > e.tn.StartSlack.Seconds()+2*e.opts.Poll.Seconds() &&
		!e.plausible(t, s.At, s.Position) && e.reached(t) < earlyRestart
	switch {
	case !mid:
		t.midSince = time.Time{}
	case t.midSince.IsZero():
		t.midSince = s.At
	case s.At.Sub(t.midSince) >= blipWindow:
		it := t.item
		e.abandon()
		e.command(ctx, "restart", func(c context.Context) error { return e.cfg.Player.SetPosition(c, 0) })
		e.waiting = &request{item: it, at: s.At}
	}
}

// abandon drops the current take without settling it: a false start that
// mixtape is about to redo. Its song stays pending.
func (e *Engine) abandon() {
	t := e.cur
	e.cur = nil
	t.endAt, t.endedByUs = e.now(), true
	e.lastEnded = t
}

// resetTake discards the observations of a false start, keeping the take
// (and whether it records) as it is. After a believable start the song
// began again at the restart; after only stale positions it may have been
// playing since the take began, so the earlier bound stays.
func (e *Engine) resetTake(s spotify.Snapshot) {
	t := e.cur
	if e.believable(t) {
		t.before = e.prevAt
	}
	t.obs, t.startedAt = nil, s.At
}

// transitional reports a snapshot taken while Spotify was switching
// tracks: the new title and duration under the previous track's URI.
func (e *Engine) transitional(s spotify.Snapshot) bool {
	t := e.cur
	return t != nil && s.Track.URI == t.key && s.Track.Name != "" && t.track.Name != "" && s.Track.Name != t.track.Name
}

// crossfadeShrink bounds how much shorter Spotify may report a song a
// moment after it starts before that is read as crossfade: with crossfade
// on, the duration drops by the crossfade time (up to 12 s).
const (
	crossfadeShrinkMin = time.Second
	crossfadeShrinkMax = 13 * time.Second
)

// refineDuration follows Spotify's updates to the current song's duration.
// A song that shrinks by a few seconds right after it starts is Spotify
// trimming it for crossfade, which would overlap every song: unless
// crossfade was allowed, the session ends at once.
func (e *Engine) refineDuration(s spotify.Snapshot) {
	t := e.cur
	d := s.Track.Duration
	if d <= 0 || d == t.track.Duration || s.State != spotify.StatePlaying {
		return
	}
	shrink := t.track.Duration - d
	if shrink >= crossfadeShrinkMin && shrink <= crossfadeShrinkMax && !e.opts.AllowCrossfade && !t.ad {
		e.fail(fmt.Errorf("%w (Spotify shortened %q by %.1fs to blend it into the next song)",
			ErrCrossfade, t.track.Name, shrink.Seconds()))
		return
	}
	t.track.Duration = d
}

// parked reports that the recording song played to its end and Spotify
// stopped on it (the end of a context) instead of moving to another track.
func (e *Engine) parked(s spotify.Snapshot) bool {
	t := e.cur
	if !t.recording || s.State == spotify.StatePlaying {
		return false
	}
	last, dur := t.lastPos(), t.track.Duration.Seconds()
	if last < dur-e.tn.EndSlack.Seconds() {
		return false
	}
	return s.State == spotify.StateStopped || s.Position < last-e.tn.Gap.Seconds() || s.Position >= dur-0.25
}

// onPlayerError handles a failed observation.
func (e *Engine) onPlayerError(s spotify.Snapshot) {
	switch {
	case isAny(s.Err, spotify.ErrNotAuthorized, spotify.ErrAppleEventsBlocked):
		e.fail(s.Err)
	case isAny(s.Err, spotify.ErrPromptPending):
		e.warn("macOS is asking whether mixtape may control Spotify: approve the prompt")
	default:
		if e.goneSince.IsZero() {
			e.goneSince = e.now()
		}
		e.warn("cannot reach Spotify: " + s.Err.Error())
	}
}

// beginTake starts following a newly observed track.
func (e *Engine) beginTake(ctx context.Context, s spotify.Snapshot) {
	t := &take{key: s.Track.URI, track: s.Track, before: e.prevAt, startedAt: s.At}
	e.cur = t
	if s.Track.IsAd() {
		t.ad = true
		return
	}
	if e.plan.Mode == ModeLive {
		e.beginLive(ctx, t, s)
		return
	}
	e.beginQueued(ctx, t, s)
}

// beginRecording marks t as recording item, restarting the song from the
// top when it was joined mid-song.
func (e *Engine) beginRecording(ctx context.Context, t *take, it *Item, s spotify.Snapshot) {
	t.item = it // a candidate until it records
	if s.State != spotify.StatePlaying {
		return // loaded but not playing yet: promoted when it starts
	}
	if s.Position > e.tn.StartSlack.Seconds()+2*e.opts.Poll.Seconds() {
		if !t.before.IsZero() && s.Position > s.At.Sub(t.before).Seconds()+e.tn.StaleSlack.Seconds() {
			return // the previous track's position under the new URI: wait for a real one
		}
		// Joined mid-song: restart it from the top in a fresh take, and give
		// Spotify time to do it before steering anything else.
		e.releaseCur()
		e.command(ctx, "restart", func(c context.Context) error { return e.cfg.Player.SetPosition(c, 0) })
		e.waiting = &request{item: it, at: s.At}
		return
	}
	e.adopt(t, it, s.At)
}

// promote starts recording a candidate take once Spotify plays it: after
// "play track" the new track appears while still paused, and only starts
// a moment later.
func (e *Engine) promote(ctx context.Context, s spotify.Snapshot) {
	t := e.cur
	if t == nil || t.recording || t.ad || t.passive || t.item == nil ||
		t.item.state != itemPending || s.State != spotify.StatePlaying || e.stopping {
		return
	}
	e.beginRecording(ctx, t, t.item, s)
}

// adopt makes t the recording of it. A take adopted after it began keeps
// its observations, so the song's start is still known.
func (e *Engine) adopt(t *take, it *Item, at time.Time) {
	t.item, t.recording = it, true
	e.waiting = nil
	e.lastActive = at
	if e.believable(t) {
		e.announce(t)
	}
}

// announce emits track_started for t once. It waits until the song is
// believably playing, so a false start leaves no trace.
func (e *Engine) announce(t *take) {
	if !t.announced {
		t.announced = true
		e.emit(Event{Type: EventTrackStarted, Track: t.eventTrack()})
	}
}

// releaseCur ends the current take as mixtape's doing, just before a
// command that changes or restarts the track: the next observation then
// starts a fresh take.
func (e *Engine) releaseCur() {
	if e.cur != nil {
		e.endTake(e.now(), true)
	}
}

// endTake closes the current take at time at.
func (e *Engine) endTake(at time.Time, byUs bool) {
	t := e.cur
	e.cur = nil
	e.lastEnded = t
	t.endAt = at
	t.endedByUs = t.endedByUs || byUs
	if t.recording {
		t.item.state = itemBusy
		e.finals = append(e.finals, t)
		e.finalizeReady(false)
	}
}

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
		e.reached(t) >= earlyRestart || it.falseStarts >= maxFalseStarts {
		return false
	}
	it.falseStarts++
	it.state = itemPending
	return true
}

// crossfadeWindow bounds how early a crossfaded song changes (Spotify's
// crossfade goes up to 12 s).
const crossfadeWindow = 13.0

// crossfaded recognizes Spotify's crossfade: consecutive songs that change
// on their own the same few seconds before their end. It ends the session
// rather than discard every song.
func (e *Engine) crossfaded(t *take, seg Segment) bool {
	gap := t.track.Duration.Seconds() - t.lastPos()
	if seg.Complete || seg.Reason != ReasonEndedEarly || t.endedByUs || t.userSkipped ||
		gap < e.tn.EndSlack.Seconds() || gap > crossfadeWindow {
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
	switch {
	case t.userSkipped:
		it.state = itemDone
		e.counts.Skipped++
		e.emit(Event{Type: EventTrackSkipped, Track: t.eventTrack(), Reason: "skipped by you"})
	case e.plan.Mode == ModeLive:
		it.state = itemDone // live mode records what plays; nothing to retry
	case it.attempts >= maxTakes:
		e.failItem(it, reason)
	}
}

// failItem gives up on a song for this session.
func (e *Engine) failItem(it *Item, reason string) {
	if it.state == itemDone || it.state == itemFailed {
		return
	}
	it.state, it.reason = itemFailed, reason
	e.counts.Failed++
	e.submit(job{kind: jobMark, item: it, status: library.StatusFailed, reason: reason})
	e.emit(Event{Type: EventTrackFailed, Track: itemTrack(it), Reason: reason})
}

// render writes a segment's frames to a WAV in the work directory.
func (e *Engine) render(t *take, seg Segment) (string, error) {
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	sp := e.spool
	if sp == nil {
		return "", errNoSpool
	}
	oldest, newest := sp.Oldest(), sp.Frames()
	ranges := make([]Range, 0, len(seg.Ranges))
	var total int64
	for _, r := range seg.Ranges {
		r.From, r.To = max(r.From, oldest), min(r.To, newest)
		if r.Len() > 0 {
			ranges = append(ranges, r)
			total += r.Len()
		}
	}
	if total == 0 {
		return "", errNoSpool
	}
	if err := os.MkdirAll(e.cfg.WorkDir, 0o700); err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.wav", safeName(t.item.Track.ID), t.startedAt.UnixNano())
	path := filepath.Join(e.cfg.WorkDir, name)
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	format := sp.Format()
	w := bufio.NewWriterSize(f, 1<<20)
	err = wav.WriteHeader(w, total*int64(format.BytesPerFrame()), format.Rate, format.Channels)
	for _, r := range ranges {
		if err != nil {
			break
		}
		var n int64
		n, err = sp.ReadRange(r.From, r.To, w)
		if err == nil && n != r.Len() {
			err = fmt.Errorf("%w: %d of %d frames", errShortRead, n, r.Len())
		}
	}
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

// emitBoundary reports the measured timing of a song (--verbose).
func (e *Engine) emitBoundary(t *take, seg Segment) {
	rate := 0
	e.spoolMu.Lock()
	if e.spool != nil {
		rate = e.spool.Format().Rate
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
		e.spoolMu.Lock()
		first := float64(seg.Anchor-e.spool.FrameAt(t.startedAt)) / float64(rate)
		e.spoolMu.Unlock()
		ev.OffsetSeconds = &first
	}
	e.emit(ev)
}

// itemTrack describes a planned song for events.
func itemTrack(it *Item) *EventTrack {
	return &EventTrack{
		ID: it.Track.ID, PlayedID: it.Track.PlayedID, Title: it.Track.Title, Artist: it.Track.Artist,
		Collection: it.Group.Spec.Name, Position: it.position(), DurationMS: it.Track.DurationMS,
		Attempt: it.attempts,
	}
}

// libraryTrack merges what the plan knew with what Spotify reported while
// the song played.
func libraryTrack(it *Item, played spotify.Track) library.Track {
	t := it.Track
	if played.ID != "" && played.ID != t.ID {
		t.PlayedID = played.ID
	}
	set := func(dst *string, v string) {
		if v != "" {
			*dst = v
		}
	}
	set(&t.Title, played.Name)
	set(&t.Artist, played.Artist)
	set(&t.Album, played.Album)
	set(&t.AlbumArtist, played.AlbumArtist)
	set(&t.ArtworkURL, played.ArtworkURL)
	if played.Duration > 0 {
		t.DurationMS = played.Duration.Milliseconds()
	}
	if t.TrackNumber == 0 || it.Group.Spec.Kind != library.KindAlbum {
		t.TrackNumber = played.TrackNumber
	}
	t.DiscNumber, t.Popularity = played.DiscNumber, played.Popularity
	return t
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
			return r
		}
		return '_'
	}, s)
}

// isAny reports whether err matches any of the targets.
func isAny(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
