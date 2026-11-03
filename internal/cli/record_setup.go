package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// recordRun is one record command once its links are resolved: what the
// setup and the session need, gathered as they are opened.
type recordRun struct {
	cmdPath  string // for usage errors
	flags    *recordFlags
	settings config.Settings
	lib      *library.Library
	plan     *engine.Plan
	args     []string // the links, as given
	player   player
	source   capture.Source // nil when nothing plays
}

// record runs a session. When every song is already done (or only needs
// copying or re-encoding) nothing plays, so neither Spotify nor audio
// capture is needed.
func (a *app) record(ctx context.Context, run *recordRun) error {
	svc := a.deps.services
	playback := needsPlayback(run.plan)
	if playback {
		if err := a.checkRecordReady(run.flags); err != nil {
			return err
		}
	}
	lockPath, err := svc.lockPath()
	if err != nil {
		return err
	}
	id := a.deps.getenv(session.EnvDetached)
	if id == "" {
		id = session.NewID(a.deps.now())
	}
	lock, err := session.Acquire(lockPath, session.LockInfo{
		PID: os.Getpid(), SessionID: id, Home: a.home, StartedAt: a.deps.now(),
	})
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	// From here a stop request (Ctrl-C, `mixtape stop`) ends the setup
	// gracefully instead of killing the process mid-way.
	setupCtx, stopGuard, stopped := a.guardSetup(ctx)
	defer stopGuard()

	run.player = svc.player()
	pending := a.recoverSpotify(setupCtx, run.player) // even when nothing plays: a copy-only run must not lose it
	unlock, err := run.lib.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	rec, err := a.newRecorder(id, run, pending)
	if err != nil {
		return err
	}
	defer rec.close()
	if playback {
		if run.source, err = a.openSource(setupCtx, run); err != nil {
			return rec.abort(err, stopped())
		}
		// The engine closes the source when it stops capturing, but a
		// session that fails before that must not leave it open.
		defer closeSource(run.source)
		rec.setCapture(run.source)
	}
	if err := engine.Prepare(run.lib, run.plan); err != nil {
		return rec.abort(err, false)
	}
	if stopped() {
		return rec.abort(context.Canceled, true)
	}
	if playback {
		// A long playlist takes hours: keep the Mac from idle-sleeping
		// through it. (Closing the lid still sleeps it.)
		release := svc.keepAwake()
		defer release()
	}
	// The guard stays until the end: the session's own handler starts inside
	// run, and a signal reaches both, so none falls in between.
	return rec.run(ctx, run)
}

// guardSetup handles stop signals while a session sets up: they cancel the
// returned context, and stopped reports whether one arrived. release ends
// the guard (it is safe to call more than once).
func (a *app) guardSetup(ctx context.Context) (setup context.Context, release func(), stopped func() bool) {
	setup, cancel := context.WithCancel(ctx)
	var got atomic.Bool
	stopWatch := a.watchSignals(func(os.Signal) {
		got.Store(true)
		cancel()
	})
	return setup, stopWatch, got.Load
}

// needsPlayback reports whether the plan records anything.
func needsPlayback(plan *engine.Plan) bool {
	return plan.Mode != engine.ModeQueue || plan.Count(engine.ActionRecord) > 0
}

// checkRecordReady checks what must hold before anything plays.
func (a *app) checkRecordReady(f *recordFlags) error {
	svc := a.deps.services
	if _, ok := svc.spotifyApp(a.deps.userHome); !ok {
		return spotify.ErrNotInstalled
	}
	// When the process list cannot be read, the poll that follows reports
	// precisely what is wrong.
	if st, _ := a.spotifyStatus(); st == spotifyStopped {
		return spotify.ErrNotRunning
	}
	if prefs, err := svc.prefs(a.deps.userHome); err == nil && !f.allowCrossfade {
		if what := blending(prefs); what != "" {
			return fmt.Errorf("%w (%s)", errCrossfade, what)
		}
	}
	if !a.afconvertFound() {
		return errNoAfconvert
	}
	return nil
}

// openSource checks Spotify answers and opens the capture backend.
func (a *app) openSource(ctx context.Context, run *recordRun) (capture.Source, error) {
	f := run.flags
	snap := run.player.Poll(ctx)
	if snap.Err != nil {
		return nil, snap.Err
	}
	if snap.Volume < 100 && !f.mute {
		a.out.Warn("Spotify's volume is %d%%: recordings are made at that level (100%% is best)", snap.Volume)
	}
	method, err := a.selectCapture(run.settings)
	if err != nil {
		return nil, err
	}
	if f.mute && method != capture.MethodTap {
		return nil, &usageError{cmdPath: run.cmdPath, err: fmt.Errorf("%w: --mute needs --capture tap", errInvalidFlag)}
	}
	if err := a.requireCapturePermission(method); err != nil {
		return nil, err
	}
	return a.deps.services.openCapture(ctx, captureRequest{
		Method: method, Device: run.settings.Device, Mute: f.mute, Bits: run.settings.Bits(),
	})
}

// capturePermission is the permission capture method m needs.
func capturePermission(m capture.Method) permission.Kind {
	if m == capture.MethodDevice {
		return permission.Microphone
	}
	return permission.SystemAudio
}

// requireCapturePermission fails fast when macOS has not granted the host
// app what the capture method needs: without it the capture is silent.
// An undecided microphone is left alone, because opening the device shows
// the system prompt.
func (a *app) requireCapturePermission(m capture.Method) error {
	k := capturePermission(m)
	st := a.deps.services.permission(k)
	if st == permission.Denied || (st == permission.Undecided && k == permission.SystemAudio) {
		app := a.deps.services.hostApp()
		return newError(ExitPermission, permission.ErrNotGranted,
			permission.Instructions(k, app)+"; 'mixtape setup' walks you through it",
			"%s may not %s yet, so the recording would be silent", app, permissionVerb(k))
	}
	return nil
}

func closeSource(src capture.Source) {
	if src != nil {
		_ = src.Close()
	}
}

// selectCapture picks the backend: the configured one, else the tap when
// this Mac supports it, else the device.
func (a *app) selectCapture(s config.Settings) (capture.Method, error) {
	return capture.Select(s.Capture, a.deps.services.tapStatus())
}

// recoverSpotify puts back Spotify settings an interrupted session left
// changed. The recorder lock is held, so that session is gone. What it
// cannot put back yet (Spotify is closed, or did not answer) is returned:
// the new session keeps it and restores it when it ends.
func (a *app) recoverSpotify(ctx context.Context, p player) *session.SpotifyState {
	st, err := session.ReadState(a.home)
	if err != nil || !st.Spotify.NeedsRestore() {
		return nil
	}
	restored, err := session.Restore(ctx, p, *st.Spotify)
	if err != nil {
		if !errors.Is(err, spotify.ErrNotRunning) {
			a.out.Warn("could not restore Spotify settings from session %s yet: %v", st.ID, err)
		}
		return st.Spotify
	}
	st.Spotify.Restored = true
	// Best effort: should this write be lost, the next session finds the
	// settings still pending, sees Spotify no longer holds what was set,
	// and leaves them alone.
	_ = session.WriteState(a.home, st)
	if len(restored) > 0 {
		a.out.Warn("restored Spotify %s changed by interrupted session %s", strings.Join(restored, ", "), st.ID)
	}
	return nil
}
