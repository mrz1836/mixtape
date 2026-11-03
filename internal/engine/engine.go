// Package engine runs a recording session: it plans the work against the
// library, steers Spotify through the songs, follows playback through
// timestamped observations, cuts each song out of the spooled capture, and
// hands complete songs to the library.
//
// One goroutine owns all session state. Audio arrives on the capture
// goroutine and goes straight into the spool; every library operation runs
// on a single worker goroutine, so encoding never stalls the observation of
// the next song and the library needs no locking.
package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spool"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// Player is the Spotify control surface the engine drives.
type Player interface {
	Poll(ctx context.Context) spotify.Snapshot
	Watch(ctx context.Context, interval time.Duration) <-chan spotify.Snapshot
	PlayTrack(ctx context.Context, trackURI, contextURI string) error
	Pause(ctx context.Context) error
	Next(ctx context.Context) error
	PlayPause(ctx context.Context) error
	SetPosition(ctx context.Context, seconds float64) error
	SetShuffle(ctx context.Context, on bool) error
	SetRepeat(ctx context.Context, on bool) error
	SetVolume(ctx context.Context, volume int) error
}

// Control is a request from the user or a signal.
type Control int

// Controls.
const (
	ControlPlayPause Control = iota // pause or resume Spotify
	ControlNext                     // skip the current song (not retried)
	ControlStop                     // end the session gracefully
)

// Errors that end a session.
var (
	// ErrSilentDevice reports that the capture device delivered only
	// silence while Spotify played: the audio is not routed to it.
	ErrSilentDevice = errors.New("the capture device is silent while Spotify plays")
	// ErrSilentTap reports that the process tap delivered only silence
	// while Spotify played: macOS has not granted audio-capture permission.
	ErrSilentTap = errors.New("the process tap is silent while Spotify plays")
	// ErrCaptureStopped reports that audio capture died mid-session.
	ErrCaptureStopped = errors.New("audio capture stopped")
	// ErrSpotifyGone reports that Spotify stayed unreachable.
	ErrSpotifyGone = errors.New("lost contact with Spotify")
	// ErrCrossfade reports songs that keep changing a few seconds before
	// their end, the signature of Spotify's crossfade.
	ErrCrossfade = errors.New("songs keep ending early: Spotify's crossfade appears to be on")
	// ErrSpoolWrite reports that captured audio could not be written to the
	// spool (usually a full disk).
	ErrSpoolWrite = errors.New("could not write the captured audio to disk")
	// ErrNoSound reports songs that kept playing without any sound reaching
	// mixtape: Spotify plays on another device, or its volume is at 0.
	ErrNoSound = errors.New("songs keep playing without any sound reaching mixtape")
	// ErrNotStarting reports songs Spotify kept not starting: signed out,
	// offline, or stuck.
	ErrNotStarting = errors.New("the songs keep not starting in Spotify")
	// ErrFailing reports songs that kept failing in a row for other reasons.
	ErrFailing = errors.New("the songs keep failing")
)

// Options tune a session.
type Options struct {
	Redo        bool
	KeepPartial bool
	KeepWAV     bool
	Artwork     bool
	// Mute is set when the tap records muted; the volume is raised to 100
	// for the session so the recording is not quiet.
	Mute bool
	// Limit ends the session after this many songs are recorded (0: none).
	Limit int
	// Match restricts live mode to titles containing this text.
	Match string
	// IdleTimeout ends the session after this long with nothing to
	// record (0: never).
	IdleTimeout time.Duration
	// Poll is the player observation interval.
	Poll time.Duration
	// StartTimeout is how long a requested song may take to start.
	StartTimeout time.Duration
	// MaxFailures ends a queue session after this many attempts in a row
	// could not be recorded (silent, not started, cut short); 0 never does.
	MaxFailures int
	// Verbose emits boundary diagnostics.
	Verbose bool
	// AllowCrossfade records even though Spotify blends songs together.
	AllowCrossfade bool
}

// Config wires a session.
type Config struct {
	Player   Player
	Source   capture.Source
	Library  *library.Library
	SpoolDir string
	WorkDir  string
	Tuning   Tuning
	Options  Options
	// Emit receives every event, on the engine goroutine.
	Emit func(Event)
	// SaveSpotify persists the player settings before the session
	// changes them, so a crash can still put them back.
	SaveSpotify func(session.SpotifyState) error
	// PendingSpotify is what an interrupted session left changed in Spotify
	// and could not put back yet; this session's restore finishes the job.
	PendingSpotify *session.SpotifyState
	Now            func() time.Time
}

// Result is how a session ended.
type Result struct {
	Outcome string // complete, incomplete or stopped
	Counts  session.Counts
	Err     error // set when the session could not continue
}

// Progress is a point-in-time view of the session, for status lines and
// session.json.
type Progress struct {
	Mode      string
	Phase     string
	State     spotify.State
	Track     *EventTrack
	Position  float64
	Duration  float64
	Recording bool
	Counts    session.Counts
	ETA       time.Duration
	Volume    int
}

// Defaults.
const (
	defaultPoll         = 150 * time.Millisecond
	defaultStartTimeout = 15 * time.Second
	maxTakes            = 2 // a song is tried at most this often
	silenceWindow       = 10 * time.Second
	silenceFloor        = 64 // |sample| at or below this, in 16-bit units at any depth (capture.Peak), counts as silence
	spotifyGoneAfter    = time.Minute
	audioWait           = 3 * time.Second // how long a finished song waits for its trailing audio
	tickEvery           = 500 * time.Millisecond
	spoolKeep           = 2 * time.Minute // audio kept before the oldest open song
	cleanupTimeout      = 15 * time.Second
	resultBuffer        = 64 // finished jobs the worker can report before it waits
)

// Engine runs one session. Create it with New and call Run once.
//
// Its state is grouped by who may touch it: the capture state is shared
// with the capture goroutine (guarded by spoolMu, or atomic), the worker
// state links the engine goroutine to the library worker, and everything
// else belongs to the engine goroutine alone, except the progress snapshot,
// which is guarded by mu.
type Engine struct {
	captureState
	workerState
	takeState
	sessionState

	cfg  Config
	opts Options
	tn   Tuning
	now  func() time.Time
	plan *Plan

	mu       sync.Mutex
	progress Progress // what Progress returns, refreshed by the engine goroutine
}

// captureState is shared with the capture goroutine, which appends to the
// spool through sink.
type captureState struct {
	spoolMu  sync.Mutex
	spool    *spool.Spool // guarded by spoolMu
	spoolErr error        // the first spool write error (guarded by spoolMu)
	// capStart is when capture began (set before it starts); loudAt is the
	// offset from it of the last block above the silence floor (−1 before
	// any).
	capStart time.Time
	loudAt   atomic.Int64
}

// workerState connects the engine goroutine to the library worker.
type workerState struct {
	jobs     *jobQueue
	results  chan result
	inflight int // jobs submitted and not yet reported (engine goroutine)
	// working mirrors inflight for other goroutines (the test simulator
	// holds its clock while library work runs).
	working atomic.Int32
	wg      sync.WaitGroup
}

// takeState is what the engine goroutine follows of Spotify's playback.
type takeState struct {
	cur       *take
	pending   []spotify.Snapshot // observations of another track, awaiting confirmation
	lastEnded *take
	finals    []*take // ended takes waiting for their audio
	last      spotify.Snapshot
	prevAt    time.Time
	waiting   *request
	follow    *followState
	// movedOff is the track mixtape just moved Spotify away from (see
	// moveOff and stillLeaving).
	movedOff string
	// earlyEnds holds how early (seconds before their end) consecutive
	// songs changed on their own, to recognize crossfade.
	earlyEnds []float64
}

// sessionState is the engine goroutine's account of the session.
type sessionState struct {
	stopping bool
	outcome  string
	fatal    error
	// streak holds the setbacks since the last song recorded (see setback).
	streak       []setbackKind
	lastSetback  string
	spotifyState session.SpotifyState
	counts       session.Counts
	recorded     int
	lastActive   time.Time
	lastTick     time.Time
	goneSince    time.Time
	warned       map[string]bool
}

// New validates cfg and returns an engine.
func New(cfg Config) (*Engine, error) {
	if cfg.Player == nil || cfg.Library == nil {
		return nil, fmt.Errorf("%w: player and library are required", errConfig)
	}
	o := cfg.Options
	if o.Poll <= 0 {
		o.Poll = defaultPoll
	}
	if o.StartTimeout <= 0 {
		o.StartTimeout = defaultStartTimeout
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Emit == nil {
		cfg.Emit = func(Event) {}
	}
	if cfg.Tuning == (Tuning{}) {
		cfg.Tuning = DefaultTuning()
	}
	e := &Engine{
		cfg: cfg, opts: o, tn: cfg.Tuning, now: cfg.Now,
		workerState:  workerState{jobs: newJobQueue(), results: make(chan result, resultBuffer)},
		sessionState: sessionState{warned: map[string]bool{}},
	}
	e.loudAt.Store(-1)
	return e, nil
}

var errConfig = errors.New("engine: invalid config")

// Progress returns the latest view of the session. Safe from any goroutine.
func (e *Engine) Progress() Progress {
	e.mu.Lock()
	defer e.mu.Unlock()
	p := e.progress
	if p.Track != nil {
		t := *p.Track
		p.Track = &t
	}
	return p
}

// Run executes plan. Controls from the user arrive on controls; canceling
// ctx forces an immediate end (Spotify settings are still restored).
func (e *Engine) Run(ctx context.Context, plan *Plan, controls <-chan Control) Result {
	e.plan = plan
	e.lastActive = e.now()
	e.counts.Total = len(plan.Items)
	e.startWorker(ctx)

	e.instantWork()
	if plan.Mode != ModeLive && e.pendingRecords() == 0 && !e.hasFollow() {
		e.drainWorker(ctx)
		return e.result(e.queueOutcome(), nil)
	}

	if err := e.setupSpotify(ctx); err != nil {
		e.drainWorker(ctx)
		return e.result(OutcomeIncomplete, err)
	}
	cleanup := context.WithoutCancel(ctx)
	defer e.restoreSpotify(cleanup)

	if err := e.startCapture(ctx); err != nil {
		e.drainWorker(ctx)
		return e.result(OutcomeIncomplete, err)
	}
	defer e.stopCapture()

	e.loop(ctx, controls)

	if e.plan.Mode != ModeLive {
		c, cancel := context.WithTimeout(cleanup, cleanupTimeout)
		e.command(c, "pause", e.cfg.Player.Pause)
		cancel()
	}
	e.drainWorker(ctx)
	return e.result(e.outcome, e.fatal)
}

// loop is the session's event loop.
func (e *Engine) loop(ctx context.Context, controls <-chan Control) {
	watchCtx, stopWatch := context.WithCancel(context.WithoutCancel(ctx))
	defer stopWatch()
	snaps := e.cfg.Player.Watch(watchCtx, e.opts.Poll)
	var events <-chan capture.Event
	if e.cfg.Source != nil {
		events = e.cfg.Source.Events()
	}
	tick := time.NewTicker(tickEvery)
	defer tick.Stop()

	for !e.finished() {
		select {
		case <-ctx.Done():
			e.stop(OutcomeStopped)
			e.forceFinalize()
			return
		case s, ok := <-snaps:
			if !ok {
				snaps = nil // a closed channel would be chosen on every turn
				e.fail(ErrSpotifyGone)
				continue
			}
			e.onSnapshot(ctx, s)
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			e.onCaptureEvent(ev)
		case c, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			e.onControl(ctx, c)
		case r := <-e.results:
			e.onResult(ctx, r)
		case <-tick.C:
			e.onTick(ctx)
		}
		e.updateProgress()
	}
}

// finished reports whether the loop can exit: the session is stopping and
// no song is still waiting for its audio.
func (e *Engine) finished() bool {
	return e.stopping && e.cur == nil && len(e.finals) == 0
}

// stop begins a graceful end with the given outcome ("" decides it from the
// songs once all work has finished). The current song is closed as cut
// short by mixtape.
func (e *Engine) stop(outcome string) {
	if e.stopping {
		return
	}
	e.stopping = true
	e.outcome = outcome
	if e.cur != nil {
		e.endTake(e.now(), true)
	}
}

// fail ends the session because it cannot continue.
func (e *Engine) fail(err error) {
	if e.fatal == nil {
		e.fatal = err
	}
	e.stop(OutcomeIncomplete)
}

func (e *Engine) onControl(ctx context.Context, c Control) {
	switch c {
	case ControlStop:
		e.stop(OutcomeStopped)
	case ControlPlayPause:
		e.command(ctx, "playpause", e.cfg.Player.PlayPause)
	case ControlNext:
		if t := e.cur; t != nil && t.item != nil {
			t.userSkipped = true
		}
		e.releaseCur()
		e.command(ctx, "next", e.cfg.Player.Next)
	}
}

// command runs a player command, reporting a failure as a warning.
func (e *Engine) command(ctx context.Context, name string, fn func(context.Context) error) {
	if err := fn(ctx); err != nil {
		e.warn(fmt.Sprintf("spotify %s: %v", name, err))
	}
}

func (e *Engine) onTick(ctx context.Context) {
	now := e.now()
	e.lastTick = now
	e.checkSpool()
	e.finalizeReady(false)
	e.checkSilence(now)
	e.pruneSpool()
	if e.stopping {
		return
	}
	// A Spotify that is gone is the more specific reason, so it is checked
	// before the idle timeout.
	if !e.goneSince.IsZero() && now.Sub(e.goneSince) > spotifyGoneAfter {
		e.fail(fmt.Errorf("%w for %s", ErrSpotifyGone, spotifyGoneAfter))
		return
	}
	if d := e.opts.IdleTimeout; d > 0 && now.Sub(e.lastActive) > d {
		e.warn(fmt.Sprintf("nothing to record for %s; ending the session", d.Round(time.Second)))
		if e.plan.Mode == ModeLive {
			e.stop(OutcomeComplete)
		} else {
			e.stop(OutcomeIncomplete)
		}
		return
	}
	if e.last.Err == nil && !e.last.At.IsZero() {
		e.steer(ctx, e.last)
	}
}

// warn emits a warning event once per distinct message.
func (e *Engine) warn(msg string) { e.warnOnce(msg, msg) }

// warnOnce emits a warning event the first time key comes up: for a
// problem whose message changes each time it recurs.
func (e *Engine) warnOnce(key, msg string) {
	if e.warned[key] {
		return
	}
	e.warned[key] = true
	e.emit(Event{Type: EventWarning, Message: msg})
}

func (e *Engine) emit(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = e.now()
	}
	e.cfg.Emit(ev)
}

// result finishes the session's accounting; fatal is why it could not
// continue, if anything. The caller emits session_finished, because only
// it knows the process exit code.
func (e *Engine) result(outcome string, fatal error) Result {
	if outcome == "" {
		outcome = e.queueOutcome()
	}
	if outcome == OutcomeComplete && e.counts.Failed > 0 {
		outcome = OutcomeIncomplete
	}
	e.counts.Remaining = e.pendingRecords()
	e.stopping = true
	e.updateProgress()
	return Result{Outcome: outcome, Counts: e.counts, Err: fatal}
}

// queueOutcome is the outcome when the work ran out.
func (e *Engine) queueOutcome() string {
	if e.counts.Failed > 0 || e.pendingRecords() > 0 {
		return OutcomeIncomplete
	}
	return OutcomeComplete
}

// pendingRecords counts planned songs not yet recorded or given up on.
func (e *Engine) pendingRecords() int {
	n := 0
	for _, it := range e.plan.Items {
		if (it.state == itemPending || it.state == itemBusy) && it.Action == ActionRecord {
			n++
		}
	}
	return n
}

func (e *Engine) hasFollow() bool {
	for _, g := range e.plan.Groups {
		if g.Follow {
			return true
		}
	}
	return false
}

// updateProgress refreshes the snapshot Progress returns.
func (e *Engine) updateProgress() {
	p := Progress{Mode: e.plan.Mode, Counts: e.counts, Volume: e.last.Volume, State: e.last.State}
	p.Counts.Remaining = e.pendingRecords()
	switch {
	case e.stopping:
		p.Phase = session.PhaseStopping
	case e.last.State == spotify.StatePlaying:
		p.Phase = session.PhaseRecording
	default:
		p.Phase = session.PhasePaused
	}
	if t := e.cur; t != nil {
		p.Track = t.eventTrack()
		p.Position = e.last.Position
		p.Duration = t.track.Duration.Seconds()
		p.Recording = t.recording
	}
	var eta time.Duration
	left := -1 // songs still to start under --limit; -1: no limit
	if e.opts.Limit > 0 {
		left = max(e.opts.Limit-e.underway(), 0)
	}
	for _, it := range e.plan.Items {
		if left == 0 {
			break
		}
		if it.state == itemPending && it.Action == ActionRecord && (e.cur == nil || e.cur.item != it) {
			eta += time.Duration(it.Track.DurationMS) * time.Millisecond
			left--
		}
	}
	if p.Recording && p.Duration > p.Position {
		eta += time.Duration((p.Duration - p.Position) * float64(time.Second))
	}
	p.ETA = eta
	e.mu.Lock()
	e.progress = p
	e.mu.Unlock()
}
