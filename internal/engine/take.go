package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/mrz1836/mixtape/internal/spotify"
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
	// silent: the song played on while capture heard nothing.
	silent bool
	// newDur is a duration Spotify reported that differs from the track's,
	// first seen at newDurAt; it is taken once it holds (see refineDuration).
	newDur   time.Duration
	newDurAt time.Time
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
	if !e.restartedAt.IsZero() && !s.At.After(e.restartedAt) {
		return // taken before mixtape started the song over: its position is stale
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
		if e.cur == t && t.recording && e.started(t) {
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
	return reachable(t.before, at, pos, e.tn)
}

// reachable reports whether a track first seen after the observation at
// before could have played to pos by at: no further than the time since,
// plus StaleSlack. Anything beyond is the previous track's position,
// reported under the new one. With no earlier observation, any position
// is.
func reachable(before, at time.Time, pos float64, tn Tuning) bool {
	return before.IsZero() || pos <= at.Sub(before).Seconds()+tn.StaleSlack.Seconds()
}

// checkResumed restarts a requested song that Spotify resumed from the
// middle. Without a context to play it in, "play track" picks a song up
// where it was last left, so it plays from there on. A stale position
// lasts a moment; one that holds for blipWindow is a resume, and the song
// is started over from the top in a fresh take.
func (e *Engine) checkResumed(ctx context.Context, t *take, s spotify.Snapshot) {
	mid := e.plan.Mode != ModeLive && t.item != nil && t.item.state == itemPending && !t.ad && !e.stopping &&
		s.State == spotify.StatePlaying && e.midSong(s.Position) && !e.plausible(t, s.At, s.Position) && !e.started(t)
	switch {
	case !mid:
		t.midSince = time.Time{}
	case t.midSince.IsZero():
		t.midSince = s.At
	case s.At.Sub(t.midSince) >= blipWindow:
		it := t.item
		e.abandon()
		e.restartFromTop(ctx, it, s.At)
	}
}

// midSong reports a position past the start of a song: beyond StartSlack
// and the two polls it can take to see a song begin.
func (e *Engine) midSong(pos float64) bool {
	return pos > e.tn.StartSlack.Seconds()+2*e.opts.Poll.Seconds()
}

// restartFromTop asks Spotify to start the current song over and waits for
// it to begin as item it. The current take has already been let go.
func (e *Engine) restartFromTop(ctx context.Context, it *Item, at time.Time) {
	e.command(ctx, "restart", func(c context.Context) error { return e.cfg.Player.SetPosition(c, 0) })
	e.waiting = &request{item: it, at: at}
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

// Spotify's crossfade lasts up to 12 s: a song that shrinks by
// crossfadeShrinkMin to crossfadeMax right after it starts, or that keeps
// changing up to crossfadeMax before its end, is being blended into the
// next.
const (
	crossfadeShrinkMin = time.Second
	crossfadeMax       = 13 * time.Second
)

// refineDuration follows Spotify's updates to the current song's duration.
// A new duration counts once Spotify reports it for blipWindow: right
// before a track change, Spotify can report the next song's duration under
// the playing song's URI and title for a poll or two, and taking it would
// judge the song by another's length.
//
// A song that shrinks by a few seconds right after it starts is Spotify
// trimming it for crossfade, which would overlap every song: unless
// crossfade was allowed, the session ends at once.
func (e *Engine) refineDuration(s spotify.Snapshot) {
	t := e.cur
	d := s.Track.Duration
	if d <= 0 || s.State != spotify.StatePlaying {
		return
	}
	switch {
	case d == t.track.Duration:
		t.newDur = 0 // the change did not hold
		return
	case d != t.newDur:
		t.newDur, t.newDurAt = d, s.At
		return
	case s.At.Sub(t.newDurAt) < blipWindow:
		return
	}
	t.newDur = 0
	shrink := t.track.Duration - d
	if shrink >= crossfadeShrinkMin && shrink <= crossfadeMax && !e.opts.AllowCrossfade && !t.ad {
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
	case errors.Is(s.Err, spotify.ErrNotAuthorized),
		// Blocked Apple Events are fatal only before Spotify ever answered.
		// Mid-session the same error is Spotify quitting or relaunching (its
		// process is still listed but no longer answers): it gets the grace
		// period of a Spotify that is gone.
		errors.Is(s.Err, spotify.ErrAppleEventsBlocked) && e.last.At.IsZero():
		e.fail(s.Err)
	case errors.Is(s.Err, spotify.ErrPromptPending):
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
	if reason := e.tooLong(s.Track.Duration); reason != "" {
		e.failItem(it, reason) // it would only fail after playing for hours
		return
	}
	t.item = it // a candidate until it records
	if s.State != spotify.StatePlaying {
		return // loaded but not playing yet: promoted when it starts
	}
	if e.midSong(s.Position) {
		if !e.plausible(t, s.At, s.Position) {
			return // the previous track's position under the new URI: wait for a real one
		}
		// Joined mid-song: restart it from the top in a fresh take, and give
		// Spotify time to do it before steering anything else.
		e.releaseCur()
		e.restartFromTop(ctx, it, s.At)
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
	if e.started(t) {
		e.announce(t)
	}
}

// started reports whether t's song has really begun: it believably played
// earlyRestart seconds, past every kind of false start.
func (e *Engine) started(t *take) bool { return e.reached(t) >= earlyRestart }

// announce emits track_started for t once. It waits until the song has
// really begun, so a false start leaves no trace.
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
