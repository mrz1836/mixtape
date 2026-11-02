package engine

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var (
	errNoSpool   = errors.New("no audio spooled for this song")
	errShortRead = errors.New("the spool returned fewer frames than requested")
)

// jobKind is a library operation run on the worker goroutine.
type jobKind int

const (
	jobCommit jobKind = iota // encode, tag and place a rendered WAV
	jobCopy                  // copy a finished song from elsewhere in the library
	jobEncode                // re-encode a kept WAV from an earlier session
	jobMark                  // record a status (failed, unavailable)
	jobLookup                // is a song decided on the fly already done?
)

// job is one library operation. Only the worker touches the library.
type job struct {
	kind    jobKind
	item    *Item
	take    *take
	played  spotify.Track
	wav     string
	partial bool
	covered time.Duration
	from    *library.Location
	status  library.Status
	reason  string
}

// result reports a finished job back to the engine goroutine.
type result struct {
	job

	entry    library.Entry
	loc      *library.Location
	inFolder bool
	err      error
}

// startWorker launches the library worker.
func (e *Engine) startWorker(ctx context.Context) {
	wctx := context.WithoutCancel(ctx) // finish in-flight encodes even when stopping
	e.wg.Go(func() {
		for j := range e.jobs {
			e.results <- e.runJob(wctx, j)
		}
	})
}

// submit queues a job for the worker.
func (e *Engine) submit(j job) {
	e.inflight++
	e.working.Add(1)
	e.jobs <- j
}

// drainWorker processes every outstanding result (which may submit more
// jobs, such as copies of a song just recorded), then stops the worker.
func (e *Engine) drainWorker(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for e.inflight > 0 {
		e.onResult(ctx, <-e.results)
	}
	close(e.jobs)
	e.wg.Wait()
}

// runJob performs one library operation.
func (e *Engine) runJob(ctx context.Context, j job) result {
	lib := e.cfg.Library
	r := result{job: j}
	c := j.item.Group.Collection
	switch j.kind {
	case jobCommit:
		t := libraryTrack(j.item, j.played)
		c.Ensure(t)
		r.entry, r.err = lib.Commit(ctx, c, t, j.wav, library.CommitOptions{
			Artwork: e.opts.Artwork, KeepWAV: e.opts.KeepWAV, Partial: j.partial,
			RecordedMS: j.covered.Milliseconds(),
		})
		if r.err == nil {
			r.loc = &library.Location{Collection: c, Entry: r.entry, Path: c.Path(&r.entry)}
		}
	case jobCopy:
		c.Ensure(j.item.Track)
		r.entry, r.err = lib.Copy(ctx, c, j.item.Track, *j.from)
		if r.err == nil {
			r.loc = &library.Location{Collection: c, Entry: r.entry, Path: c.Path(&r.entry)}
		}
	case jobEncode:
		r.entry, r.err = lib.RetryEncode(ctx, c, j.item.Track.ID, library.CommitOptions{
			Artwork: e.opts.Artwork, KeepWAV: e.opts.KeepWAV,
		})
	case jobMark:
		c.Ensure(j.item.Track)
		r.err = lib.Mark(c, j.item.Track.ID, j.status, j.reason)
	case jobLookup:
		c.Ensure(j.item.Track)
		r.inFolder = c.Done(j.item.Track.ID)
		for _, loc := range lib.Find(j.item.Track.ID) {
			if loc.Collection.Dir != c.Dir {
				r.from = &loc
				break
			}
		}
		if !r.inFolder && r.from == nil {
			r.err = lib.Save(c) // the new pending entry
		}
	}
	return r
}

// onResult applies a finished job on the engine goroutine.
func (e *Engine) onResult(ctx context.Context, r result) {
	e.inflight--
	e.working.Add(-1)
	it := r.item
	switch r.kind {
	case jobLookup:
		if r.err != nil {
			e.warn("library: " + r.err.Error())
		}
		e.onLookup(ctx, r)
	case jobMark:
		if r.err != nil {
			e.warn("library: " + r.err.Error())
		}
	case jobCommit, jobEncode:
		e.onSaved(r)
	case jobCopy:
		if r.err != nil {
			e.failItem(it, "copy: "+r.err.Error())
			return
		}
		it.state = itemDone
		e.counts.Copied++
		ev := e.entryEvent(it, r.entry)
		if r.from != nil {
			ev.CopiedFrom = r.from.Collection.Folder() + "/" + r.from.Entry.File
		}
		e.emit(Event{Type: EventTrackCopied, Track: ev})
		e.copyTwins(it, r.loc)
	}
}

// onSaved handles a committed (or re-encoded) recording.
func (e *Engine) onSaved(r result) {
	it := r.item
	if r.err != nil {
		if errors.Is(r.err, library.ErrEncode) {
			// The library kept the WAV and marked the song encode_failed;
			// the next run encodes it again without recording.
			it.state, it.reason = itemFailed, r.err.Error()
			e.counts.Failed++
			e.emit(Event{Type: EventTrackFailed, Track: itemTrack(it), Reason: r.err.Error()})
			return
		}
		if it.state == itemBusy {
			it.state = itemPending
		}
		e.failItem(it, "save: "+r.err.Error())
		return
	}
	ev := e.entryEvent(it, r.entry)
	ev.Partial = r.partial
	e.emit(Event{Type: EventTrackSaved, Track: ev})
	if r.partial {
		return // the song stays pending: a complete take replaces it
	}
	it.state = itemDone
	e.counts.Recorded++
	e.recorded++
	e.copyTwins(it, r.loc)
	if e.limitReached() && e.plan.Mode == ModeLive {
		e.stop("") // --limit: enough songs recorded
	}
}

// copyTwins copies a song just finished into the other folders of the plan
// that list it.
func (e *Engine) copyTwins(it *Item, loc *library.Location) {
	if loc == nil {
		return
	}
	for _, other := range e.plan.Items {
		if other.Twin == it && other.state == itemPending {
			other.From = loc
			e.submit(job{kind: jobCopy, item: other, from: loc})
		}
	}
}

// entryEvent describes a saved entry.
func (e *Engine) entryEvent(it *Item, en library.Entry) *EventTrack {
	ev := itemTrack(it)
	ev.PlayedID = en.PlayedID
	if en.Title != "" {
		ev.Title, ev.Artist = en.Title, en.Artist
	}
	ev.File = filepath.Join(it.Group.Collection.Folder(), en.File)
	ev.RecordedMS, ev.AudioSHA256 = en.RecordedMS, en.AudioSHA256
	return ev
}

// instantWork queues everything that needs no playback: skips, copies
// from the library, and re-encodes of kept WAVs.
func (e *Engine) instantWork() {
	for _, it := range e.plan.Items {
		switch it.Action {
		case ActionSkip:
			it.state = itemDone
			e.counts.Skipped++
			e.emit(Event{Type: EventTrackSkipped, Track: itemTrack(it), Reason: "already in the library"})
		case ActionCopy:
			if it.From != nil {
				e.submit(job{kind: jobCopy, item: it, from: it.From})
			}
		case ActionEncode:
			e.submit(job{kind: jobEncode, item: it})
		case ActionRecord:
		}
	}
}

// submitCommit hands a rendered take to the worker.
func (e *Engine) submitCommit(it *Item, t *take, wavPath string, partial bool, covered time.Duration) {
	e.submit(job{kind: jobCommit, item: it, take: t, played: t.track, wav: wavPath, partial: partial, covered: covered})
}
