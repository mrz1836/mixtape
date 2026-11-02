package engine

import (
	"context"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// request is a PlayTrack mixtape issued and is waiting to see start.
type request struct {
	item *Item
	at   time.Time
}

// relinkTolerance is how far a relinked track's duration may differ.
const relinkTolerance = 2 * time.Second

// beginQueued decides what a new track means in queue and follow mode.
func (e *Engine) beginQueued(ctx context.Context, t *take, s spotify.Snapshot) {
	it := e.match(s.Track)
	if it == nil && e.follow != nil && e.follow.active {
		it = e.followTrack(ctx, t, s)
	}
	if it == nil || e.limitReached() {
		return
	}
	if w := e.waiting; w != nil && w.item != it {
		// Spotify jumped past the requested song: it could not play it.
		if w.item.Group == it.Group && w.item.position() < it.position() {
			e.unavailable(w.item, "Spotify skipped it (not playable here)")
		}
	}
	e.waiting = nil
	e.beginRecording(ctx, t, it, s)
}

// match finds the pending planned song a track is: by requested ID, by
// played (relinked) ID, or, for the song mixtape is waiting for, by title,
// artist and duration.
func (e *Engine) match(tr spotify.Track) *Item {
	if tr.ID == "" {
		return nil
	}
	for _, it := range e.plan.Items {
		if it.state == itemPending && it.Action == ActionRecord && (it.Track.ID == tr.ID || it.Track.PlayedID == tr.ID) {
			return it
		}
	}
	if w := e.waiting; w != nil && relinked(w.item.Track, tr) {
		w.item.Track.PlayedID = tr.ID
		return w.item
	}
	return nil
}

// relinked reports whether the played track is the requested song under
// another ID: same normalized title and lead artist, duration within 2 s.
func relinked(want library.Track, got spotify.Track) bool {
	if normalize(want.Title) != normalize(got.Name) || leadArtist(want.Artist) != leadArtist(got.Artist) {
		return false
	}
	if want.DurationMS == 0 || got.Duration == 0 {
		return true
	}
	d := time.Duration(math.Abs(float64(want.DurationMS)*float64(time.Millisecond) - float64(got.Duration)))
	return d <= relinkTolerance
}

// normalize folds case and punctuation so near-identical titles compare
// equal.
func normalize(s string) string {
	var b strings.Builder
	space := false
	for _, r := range strings.ToLower(s) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
			space = false
		case !space && b.Len() > 0:
			b.WriteByte(' ')
			space = true
		}
	}
	return strings.TrimSpace(b.String())
}

// leadArtist is the first of a comma- or ampersand-separated artist list.
func leadArtist(s string) string {
	if i := strings.IndexAny(s, ",&"); i >= 0 {
		s = s[:i]
	}
	return normalize(s)
}

// steer moves Spotify on when it is not playing something to record.
func (e *Engine) steer(ctx context.Context, s spotify.Snapshot) {
	if e.plan.Mode == ModeLive || e.stopping {
		return
	}
	t := e.cur
	if t != nil && (t.ad || t.recording) {
		return
	}
	if e.userPaused(s) {
		return
	}
	if w := e.waiting; w != nil {
		limit := e.opts.StartTimeout
		if t != nil && t.ad {
			limit += adGrace
		}
		if s.At.Sub(w.at) < limit {
			return
		}
		e.waiting = nil
		if w.item.attempts >= maxTakes {
			e.failItem(w.item, "Spotify did not start it")
		} else {
			w.item.attempts++
		}
	}
	if e.opts.Limit > 0 && e.recorded >= e.opts.Limit {
		e.stop(OutcomeComplete) // --limit reached
		return
	}
	if e.limitReached() || e.busy() {
		return // the last songs are still being saved; nothing to play yet
	}
	next := e.nextRecord()
	if next == nil {
		if !e.steerFollow(ctx, s) {
			e.stop("")
		}
		return
	}
	e.request(ctx, next, s.At)
}

// userPaused reports a pause the user made while a song was underway;
// mixtape does not take over a paused player.
func (e *Engine) userPaused(s spotify.Snapshot) bool {
	if s.State == spotify.StatePlaying {
		return false
	}
	t := e.cur
	if t == nil || !t.sawPlaying {
		return false // nothing was playing this session: take over
	}
	pos, dur := t.lastPos(), t.track.Duration.Seconds()
	return pos > 0 && pos < dur-e.tn.EndSlack.Seconds()
}

// nextRecord is the first planned song still to record.
func (e *Engine) nextRecord() *Item {
	for _, it := range e.plan.Items {
		if it.state == itemPending && it.Action == ActionRecord {
			return it
		}
	}
	return nil
}

// request asks Spotify to play it in its collection's context.
func (e *Engine) request(ctx context.Context, it *Item, at time.Time) {
	e.releaseCur()
	err := e.cfg.Player.PlayTrack(ctx, spotify.TrackURI(it.Track.ID), it.Group.ContextURI)
	if err != nil {
		e.warn("spotify play: " + err.Error())
	}
	e.waiting = &request{item: it, at: at}
	e.lastActive = at
}

// unavailable records that a song cannot be played here.
func (e *Engine) unavailable(it *Item, reason string) {
	if it.state != itemPending {
		return
	}
	it.state, it.reason = itemFailed, reason
	e.counts.Failed++
	e.submit(job{kind: jobMark, item: it, status: library.StatusUnavailable, reason: reason})
	e.emit(Event{Type: EventTrackFailed, Track: itemTrack(it), Reason: reason})
}

// limitReached reports that --limit songs are recorded or on their way:
// nothing new may start.
func (e *Engine) limitReached() bool {
	return e.opts.Limit > 0 && e.underway() >= e.opts.Limit
}

// underway counts songs recorded this session plus those still being cut
// and saved, and the one recording now.
func (e *Engine) underway() int {
	n := e.recorded
	for _, it := range e.plan.Items {
		if it.state == itemBusy {
			n++
		}
	}
	if e.cur != nil && e.cur.recording {
		n++
	}
	return n
}

// ---------- follow mode ----------

// followState tracks a playlist whose full track list is unknown: after
// its known songs, mixtape follows the playlist in Spotify (shuffle off,
// repeat on) until it loops back to its first two songs.
type followState struct {
	group      *Group
	active     bool
	first      string // ID of the playlist's first song
	second     string
	lastID     string // the previous track seen while following
	skipNextOf string // press next when this track begins
	startedAt  time.Time
}

// steerFollow starts or continues following; it reports false when there
// is nothing left to follow.
func (e *Engine) steerFollow(ctx context.Context, s spotify.Snapshot) bool {
	if e.follow != nil && e.follow.active {
		if s.State != spotify.StatePlaying && s.At.Sub(e.follow.startedAt) > e.opts.StartTimeout {
			e.follow.active = false // the playlist stopped: done
			return false
		}
		return true
	}
	g := e.nextFollowGroup()
	if g == nil {
		return false
	}
	fs := &followState{group: g, active: true, startedAt: s.At}
	if n := len(g.Items); n > 0 && e.flowedOn(g.Items[n-1]) {
		// The last known song just played out: Spotify is already moving
		// on through the playlist, so follow from here.
		fs.first = g.Items[0].Track.ID
		if n > 1 {
			fs.second = g.Items[1].Track.ID
		}
		fs.lastID = g.Items[n-1].Track.ID
		e.follow = fs
		if t := e.cur; t != nil && !t.ad && !t.recording {
			if it := e.followTrack(ctx, t, s); it != nil {
				e.adopt(t, it, s.At)
			}
		}
		return true
	}
	if n := len(g.Items); n > 0 {
		fs.first = g.Items[0].Track.ID
		if n > 1 {
			fs.second = g.Items[1].Track.ID
		}
		last := g.Items[n-1]
		fs.skipNextOf = last.Track.ID
		e.releaseCur()
		e.command(ctx, "play", func(c context.Context) error {
			return e.cfg.Player.PlayTrack(c, spotify.TrackURI(last.Track.ID), g.ContextURI)
		})
	} else {
		e.releaseCur()
		e.command(ctx, "play", func(c context.Context) error {
			return e.cfg.Player.PlayTrack(c, g.ContextURI, "")
		})
	}
	e.follow = fs
	e.lastActive = s.At
	return true
}

func (e *Engine) nextFollowGroup() *Group {
	for _, g := range e.plan.Groups {
		if g.Follow && (e.follow == nil || e.groupIndex(g) > e.groupIndex(e.follow.group)) {
			return g
		}
	}
	return nil
}

func (e *Engine) groupIndex(g *Group) int {
	for i, x := range e.plan.Groups {
		if x == g {
			return i
		}
	}
	return -1
}

// followTrack handles a new track while following: skip what is known,
// detect the loop, and add new songs to the plan.
func (e *Engine) followTrack(ctx context.Context, t *take, s spotify.Snapshot) *Item {
	fs := e.follow
	id := s.Track.ID
	prev := fs.lastID
	fs.lastID = id
	switch {
	case id == "":
		return nil
	case id == fs.skipNextOf:
		fs.skipNextOf = ""
		e.next(ctx)
		return nil
	case fs.first != "" && id == fs.second && prev == fs.first:
		fs.active = false // looped: the whole playlist has been seen
		e.stop("")
		return nil
	case fs.first == "":
		fs.first = id
	case fs.second == "" && prev == fs.first:
		fs.second = id
	}
	if it := fs.group.find(id); it != nil {
		if it.state != itemPending {
			e.next(ctx) // known and handled: move on
		}
		return nil
	}
	it := &Item{
		Group:  fs.group,
		Track:  library.Track{ID: id, Title: s.Track.Name, Artist: s.Track.Artist, DurationMS: s.Track.Duration.Milliseconds()},
		Action: ActionRecord,
	}
	fs.group.Items = append(fs.group.Items, it)
	e.plan.Items = append(e.plan.Items, it)
	e.counts.Total++
	e.submit(job{kind: jobLookup, item: it, take: t})
	return it
}

// next skips the current track.
func (e *Engine) next(ctx context.Context) {
	e.releaseCur()
	e.command(ctx, "next", e.cfg.Player.Next)
}

// ---------- live mode ----------

// beginLive decides what to do with a track in live mode: record it,
// leave it alone (--match), or skip it once the library says it is done.
func (e *Engine) beginLive(ctx context.Context, t *take, s spotify.Snapshot) {
	if m := e.opts.Match; m != "" && !strings.Contains(strings.ToLower(s.Track.Name), strings.ToLower(m)) {
		t.passive = true
		return
	}
	if s.Track.ID == "" {
		t.passive = true // a local file or podcast: nothing to record
		return
	}
	g := e.plan.Groups[0]
	it := g.find(s.Track.ID)
	if it == nil {
		it = &Item{
			Group: g, Action: ActionRecord,
			Track: library.Track{ID: s.Track.ID, Title: s.Track.Name, Artist: s.Track.Artist, DurationMS: s.Track.Duration.Milliseconds()},
		}
		g.Items = append(g.Items, it)
		e.plan.Items = append(e.plan.Items, it)
		e.counts.Total++
		if !e.opts.Redo {
			e.submit(job{kind: jobLookup, item: it, take: t})
		}
	} else if it.state == itemBusy {
		t.passive = true // the song just recorded, seen again as Spotify moves on
		return
	} else if it.state != itemPending || e.limitReached() {
		e.next(ctx) // already handled this session
		return
	}
	if e.limitReached() {
		return
	}
	e.beginRecording(ctx, t, it, s)
}

// onLookup applies the library's answer for a song decided on the fly:
// already in the folder → skip it; complete elsewhere → copy it; either
// way Spotify moves on. Otherwise recording continues.
func (e *Engine) onLookup(ctx context.Context, r result) {
	it := r.item
	if it.state != itemPending || (!r.inFolder && r.from == nil) {
		return
	}
	switch {
	case r.inFolder:
		it.state = itemDone
		e.counts.Skipped++
		e.emit(Event{Type: EventTrackSkipped, Track: itemTrack(it), Reason: "already in the library"})
	default:
		it.Action, it.From = ActionCopy, r.from
		e.submit(job{kind: jobCopy, item: it, from: r.from})
	}
	if t := e.cur; t != nil && t.item == it {
		t.recording, t.item = false, nil
		e.next(ctx)
	}
}

// flowedOn reports whether it was the song that just ended naturally,
// with Spotify continuing in its context.
func (e *Engine) flowedOn(it *Item) bool {
	t := e.lastEnded
	return t != nil && t.item == it && !t.endedByUs
}

// busy reports whether a recorded song is still waiting to be settled.
func (e *Engine) busy() bool {
	for _, it := range e.plan.Items {
		if it.state == itemBusy {
			return true
		}
	}
	return false
}
