package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
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
	kind jobKind
	item *Item
	// track is the item's song as it was when the job was queued: the
	// engine may still update the item (a relinked ID) while the worker
	// runs, so the worker reads only this copy.
	track   library.Track
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

	entry      library.Entry
	loc        *library.Location
	inFolder   bool
	otherCodec bool // a lookup found the song only in another codec
	err        error
}

// jobQueue is the worker's unbounded queue. Submitting never blocks: the
// engine goroutine can queue any number of jobs (hundreds of copies at the
// start of a run) while the worker waits for it to take results.
type jobQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	jobs   []job
	closed bool
}

func newJobQueue() *jobQueue {
	q := &jobQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *jobQueue) push(j job) {
	q.mu.Lock()
	q.jobs = append(q.jobs, j)
	q.mu.Unlock()
	q.cond.Signal()
}

// pop waits for the next job; ok is false once the queue is closed and empty.
func (q *jobQueue) pop() (job, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.jobs) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.jobs) == 0 {
		return job{}, false
	}
	j := q.jobs[0]
	q.jobs[0] = job{}
	q.jobs = q.jobs[1:]
	return j, true
}

func (q *jobQueue) close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	q.cond.Broadcast()
}

// startWorker launches the library worker.
func (e *Engine) startWorker(ctx context.Context) {
	wctx := context.WithoutCancel(ctx) // finish in-flight encodes even when stopping
	e.wg.Go(func() {
		for {
			j, ok := e.jobs.pop()
			if !ok {
				return
			}
			e.results <- e.runJob(wctx, j)
		}
	})
}

// submit queues a job for the worker; it never blocks.
func (e *Engine) submit(j job) {
	e.inflight++
	e.working.Add(1)
	j.track = j.item.Track
	e.jobs.push(j)
}

// drainWorker processes every outstanding result (which may submit more
// jobs, such as copies of a song just recorded), then stops the worker.
func (e *Engine) drainWorker(ctx context.Context) {
	ctx = context.WithoutCancel(ctx)
	for e.inflight > 0 {
		e.onResult(ctx, <-e.results)
	}
	e.jobs.close()
	e.wg.Wait()
}

// runJob performs one library operation.
func (e *Engine) runJob(ctx context.Context, j job) result {
	lib := e.cfg.Library
	r := result{job: j}
	c := j.item.Group.Collection
	switch j.kind {
	case jobCommit:
		t := libraryTrack(j.item, j.track, j.played)
		c.Ensure(t)
		r.entry, r.err = lib.Commit(ctx, c, t, j.wav, library.CommitOptions{
			Artwork: e.opts.Artwork, KeepWAV: e.opts.KeepWAV, Partial: j.partial,
			RecordedMS: j.covered.Milliseconds(),
		})
		r.loc = located(c, r.entry, r.err)
	case jobCopy:
		c.Ensure(j.track)
		r.entry, r.err = lib.Copy(ctx, c, j.track, *j.from)
		r.loc = located(c, r.entry, r.err)
	case jobEncode:
		r.entry, r.err = lib.RetryEncode(ctx, c, j.track.ID, library.CommitOptions{
			Artwork: e.opts.Artwork, KeepWAV: e.opts.KeepWAV,
		})
	case jobMark:
		c.Ensure(j.track)
		r.err = lib.Mark(c, j.track.ID, j.status, j.reason)
	case jobLookup:
		c.Ensure(j.track)
		r.inFolder = c.Done(j.track.ID)
		r.from, r.otherCodec = copySource(lib, c, j.track.ID)
		if !r.inFolder && r.from == nil {
			r.err = lib.Save(c) // the new pending entry
		}
	}
	return r
}

// located is where a stored entry now lives, or nil when storing failed.
func located(c *library.Collection, en library.Entry, err error) *library.Location {
	if err != nil {
		return nil
	}
	return &library.Location{Collection: c, Entry: en, Path: c.Path(&en)}
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
		if errors.Is(r.err, library.ErrStore) {
			e.storeFailed(it, r.err)
			return
		}
		if r.err != nil {
			e.failItem(it, "copy: "+r.err.Error())
			return
		}
		it.state = itemDone
		e.counts.Copied++
		ev := e.entryEvent(it, r.entry)
		if r.from != nil {
			ev.CopiedFrom = r.from.RelPath()
		}
		e.emit(Event{Type: EventTrackCopied, Track: ev})
		e.copyTwins(it, r.loc)
	}
}

// onSaved handles a committed (or re-encoded) recording.
func (e *Engine) onSaved(r result) {
	it := r.item
	switch {
	case errors.Is(r.err, library.ErrKeptComplete):
		// The partial take was not kept: the complete file stays.
		e.counts.Discarded++
		e.emit(Event{Type: EventTrackDiscarded, Track: itemTrack(it), Reason: r.err.Error()})
		return
	case errors.Is(r.err, library.ErrStore):
		e.storeFailed(it, r.err)
		return
	case errors.Is(r.err, library.ErrEncode):
		// The library kept the WAV and marked the song encode_failed; the
		// next run encodes it again without recording.
		e.markFailed(it, r.err.Error())
		return
	case r.err != nil:
		it.unbusy()
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
	e.streak = e.streak[:0] // a song made it: whatever went wrong before has passed
	e.copyTwins(it, r.loc)
	if e.limitReached() && e.plan.Mode == ModeLive {
		e.stop("") // --limit: enough songs recorded
	}
}

// storeFailed ends the session when the library cannot take files (an
// unplugged drive, a full disk): every further song would be lost the same
// way. The song stays pending for the next run.
func (e *Engine) storeFailed(it *Item, err error) {
	it.unbusy()
	e.fail(fmt.Errorf("%s: %w", it.Track.Title, err))
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
	ev.RecordedMS, ev.AudioSHA256, ev.Codec = en.RecordedMS, en.AudioSHA256, string(en.Codec)
	return ev
}

// instantWork queues everything that needs no playback: skips, copies
// from the library, and re-encodes of kept WAVs.
func (e *Engine) instantWork() {
	for _, it := range e.plan.Items {
		switch it.Action {
		case ActionSkip:
			e.skip(it, itemTrack(it), reasonInLibrary)
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
	e.submit(job{kind: jobCommit, item: it, played: t.track, wav: wavPath, partial: partial, covered: covered})
}

// libraryTrack merges what the plan knew with what Spotify reported while
// the song played.
func libraryTrack(it *Item, base library.Track, played spotify.Track) library.Track {
	t := base
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
