package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
)

// recorder connects one engine session to the outputs: stdout (text or
// NDJSON), the session's event log, session.json, and the terminal UI.
type recorder struct {
	a      *app
	id     string
	log    *session.EventLog
	mu     sync.Mutex
	state  session.State
	ui     *ttyUI
	eng    *engine.Engine
	saved  time.Time
	closed bool
	// stateFailed and logFailed report a failed write of session.json or
	// the event log, so each is warned about once.
	stateFailed, logFailed bool
}

// newRecorder starts the record of session id: its event log and
// session.json. pending is the Spotify state an interrupted session left
// to put back, if any.
func (a *app) newRecorder(id string, run *recordRun, pending *session.SpotifyState) (*recorder, error) {
	log, err := session.OpenEventLog(session.EventsPath(a.home, id))
	if err != nil {
		return nil, err
	}
	now := a.deps.now()
	captureName := "none (nothing to play)"
	if run.source != nil {
		captureName = captureLabel(run.source)
	}
	r := &recorder{a: a, id: id, log: log, state: session.State{
		ID: id, PID: os.Getpid(), Phase: session.PhaseStarting, Mode: run.plan.Mode,
		StartedAt: now, UpdatedAt: now, Library: run.settings.Library,
		Capture: captureName, Targets: run.args,
		Events: log.Path(), Counts: session.Counts{Total: len(run.plan.Items)},
		Spotify: pending, // kept until a session puts it back
	}}
	if a.deps.getenv(session.EnvDetached) != "" {
		r.state.Log = session.LogPath(a.home, id)
	}
	r.write()
	return r, nil
}

func captureLabel(src capture.Source) string { return string(src.Method()) + " (" + src.Name() + ")" }

// setCapture records the capture backend once it is open.
func (r *recorder) setCapture(src capture.Source) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Capture = captureLabel(src)
	r.write()
}

// abort ends a session that failed (or was stopped) before it ran, so its
// state says how it ended rather than "starting".
func (r *recorder) abort(err error, stopped bool) error {
	r.mu.Lock()
	counts := r.state.Counts
	r.mu.Unlock()
	counts.Remaining = counts.Total
	res := engine.Result{Outcome: engine.OutcomeIncomplete, Counts: counts, Err: err}
	if stopped {
		res = engine.Result{Outcome: engine.OutcomeStopped, Counts: counts}
	}
	exit, failure := outcomeExit(res, false)
	r.finish(res, exit, failure)
	return failure
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.closed {
		r.closed = true
		_ = r.log.Close()
	}
}

// write saves session.json. A failure is reported once: the session goes
// on, but status, wait and stop cannot follow it.
func (r *recorder) write() {
	r.state.UpdatedAt = r.a.deps.now()
	if err := session.WriteState(r.a.home, r.state); err != nil && !r.stateFailed {
		r.stateFailed = true
		r.warn("could not save the session state (status, wait and stop will not see it): %v", err)
	}
}

// warn reports a recorder problem above the status line, or on stderr.
func (r *recorder) warn(format string, args ...any) {
	if r.ui != nil {
		r.ui.log(r.a.out.paint(colorYellow, "! "+fmt.Sprintf(format, args...)))
		return
	}
	r.a.out.Warn(format, args...)
}

// run executes the session and reports how it ended.
func (r *recorder) run(ctx context.Context, run *recordRun) error {
	a := r.a
	f, s, plan := run.flags, run.settings, run.plan
	eng, err := engine.New(engine.Config{
		Player: run.player, Source: run.source, Library: run.lib,
		SpoolDir: filepath.Join(a.home, "spool"), WorkDir: filepath.Join(a.home, "work"),
		Options: engine.Options{
			Redo: f.redo, KeepPartial: f.keepPartial, KeepWAV: f.keepWAV, Artwork: s.Artwork, Mute: f.mute,
			Limit: f.limit, Match: f.match, IdleTimeout: s.IdleTimeout, Verbose: a.out.Verbose(),
			AllowCrossfade: f.allowCrossfade, MaxFailures: s.MaxFailures,
		},
		Emit:           r.emit,
		SaveSpotify:    r.saveSpotify,
		PendingSpotify: r.pendingSpotify(),
		Now:            a.deps.now,
	})
	if err != nil {
		return r.abort(err, false)
	}
	r.eng = eng

	started := engine.Event{
		Type: engine.EventSessionStarted, Mode: plan.Mode, Capture: r.state.Capture,
		Library: s.Library, PID: os.Getpid(), Codec: string(s.Codec),
	}
	if run.source != nil {
		f := run.source.Format()
		started.BitDepth, started.SampleRate = s.Bits(), f.Rate
	}
	r.emit(started)
	r.emit(engine.Event{Type: engine.EventPlan, Items: plan.Summary()})

	runCtx, force := context.WithCancel(context.WithoutCancel(ctx))
	defer force()
	controls := make(chan engine.Control, 8)
	stopSignals := a.watchSignals(stopOnSignal(controls, force))
	defer stopSignals()

	if a.out.Interactive() && a.deps.stdinTTY && a.deps.stdinFile != nil {
		ui, uiErr := startTTY(a, eng, controls, force)
		if uiErr == nil {
			r.ui = ui
			defer ui.close()
		}
	}
	r.state.Phase = session.PhaseRecording
	r.write()
	stopProgress := r.keepProgress()
	res := eng.Run(runCtx, plan, controls)
	if errors.Is(res.Err, engine.ErrSilentTap) && a.deps.services.permission(permission.SystemAudio) == permission.Granted {
		res.Err = errTapHearsNothing
	}
	stopProgress()
	if r.ui != nil {
		r.ui.close()
	}
	exit, failure := outcomeExit(res, runCtx.Err() != nil)
	r.finish(res, exit, failure)
	return failure
}

// outcomeExit maps an engine result to the exit code and the error to
// report (nil on success).
func outcomeExit(res engine.Result, forced bool) (int, error) {
	switch {
	case forced:
		return ExitInterrupted, newError(ExitInterrupted, context.Canceled, "", "forced to quit; the song in progress was discarded")
	case res.Err != nil:
		e := mapErr(res.Err)
		return e.Exit, e
	case res.Outcome == engine.OutcomeIncomplete:
		n := res.Counts.Failed + res.Counts.Remaining
		return ExitIncomplete, newError(ExitIncomplete, nil,
			"run the same command again to retry; 'mixtape list <link>' shows each song's status",
			"finished with %d song(s) not recorded", n)
	}
	return ExitOK, nil
}

// finish records the end of the session everywhere.
func (r *recorder) finish(res engine.Result, exit int, failure error) {
	counts := res.Counts
	ev := engine.Event{Type: engine.EventSessionFinished, Result: res.Outcome, Counts: &counts, ExitCode: &exit}
	if failure != nil {
		ev.Message = failure.Error()
	}
	r.mu.Lock()
	now := r.a.deps.now()
	r.state.Phase, r.state.Result, r.state.ExitCode = session.PhaseFinished, res.Outcome, &exit
	r.state.FinishedAt, r.state.Current, r.state.Counts, r.state.ETASeconds = &now, nil, counts, 0
	if res.Err != nil {
		r.state.Result, r.state.Error = session.ResultError, res.Err.Error()
	}
	r.write()
	r.mu.Unlock()
	r.emit(ev)
	if !r.a.out.JSON() {
		r.a.out.Println("%s", summaryLine(res, r.a.out))
	}
}

// emit fans an engine event out to the outputs. It runs on the engine
// goroutine (and once on the command goroutine before and after).
func (r *recorder) emit(ev engine.Event) {
	ev.SessionID = r.id
	if ev.Time.IsZero() {
		ev.Time = r.a.deps.now()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	if err := r.log.Write(ev); err != nil && !r.logFailed {
		r.logFailed = true
		r.warn("could not write the session's event log (status --follow will miss events): %v", err)
	}
	r.track(ev)
	if r.a.out.JSON() {
		if ev.Type != engine.EventBoundary || r.a.out.Verbose() {
			_ = r.a.out.Event(ev)
		}
		return
	}
	if line := eventLine(ev, r.a.out); line != "" {
		if r.ui != nil {
			r.ui.log(line)
		} else {
			r.a.out.Println("%s", line)
		}
	}
}

// track keeps session.json in step with the event stream.
func (r *recorder) track(ev engine.Event) {
	if ev.Type != engine.EventSessionFinished {
		r.refresh()
	}
	now := r.a.deps.now()
	if ev.Type != engine.EventWarning && ev.Type != engine.EventBoundary || now.Sub(r.saved) > 2*time.Second {
		r.saved = now
		r.write()
	}
}

// refresh copies the engine's progress into the session state.
func (r *recorder) refresh() {
	if r.eng == nil {
		return
	}
	p := r.eng.Progress()
	r.state.Counts = p.Counts
	r.state.ETASeconds = int(p.ETA.Seconds())
	r.state.Current = nil
	if p.Track != nil {
		r.state.Current = &session.Current{
			TrackID: p.Track.ID, Title: p.Track.Title, Artist: p.Track.Artist,
			Collection: p.Track.Collection, Position: p.Position, Duration: p.Duration, Recording: p.Recording,
		}
	}
	if p.Phase == session.PhasePaused || p.Phase == session.PhaseRecording {
		r.state.Phase = p.Phase
	}
}

// keepProgress refreshes session.json every couple of seconds, so status
// shows the current song and position between events.
func (r *recorder) keepProgress() func() {
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		t := time.NewTicker(progressEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				r.mu.Lock()
				if !r.closed {
					r.refresh()
					r.write()
				}
				r.mu.Unlock()
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

// progressEvery is how often session.json is refreshed during a session.
const progressEvery = 2 * time.Second

// pendingSpotify is the Spotify state an interrupted session left to put
// back, if any.
func (r *recorder) pendingSpotify() *session.SpotifyState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.state.Spotify.NeedsRestore() {
		return nil
	}
	st := *r.state.Spotify
	return &st
}

// saveSpotify persists the player settings the session is about to
// change (and later, that they were restored).
func (r *recorder) saveSpotify(st session.SpotifyState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Spotify = &st
	r.state.UpdatedAt = r.a.deps.now()
	return session.WriteState(r.a.home, r.state)
}

// stopOnSignal turns SIGINT and SIGTERM into a graceful stop, and a
// second SIGINT into a forced quit. Its count is touched only by the one
// goroutine of watchSignals.
func stopOnSignal(controls chan<- engine.Control, force context.CancelFunc) func(os.Signal) {
	interrupts := 0
	return func(sig os.Signal) {
		if sig == os.Interrupt {
			interrupts++
		}
		if interrupts >= 2 {
			force()
			return
		}
		select {
		case controls <- engine.ControlStop:
		default:
		}
	}
}
