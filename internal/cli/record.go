package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// recordFlags holds `record`'s own flags.
type recordFlags struct {
	detach, dryRun, redo, keepPartial, keepWAV, mute, allowCrossfade bool
	artwork                                                          bool
	limit, maxFailures                                               int
	match, into, capture, device                                     string
	idleTimeout                                                      time.Duration
}

// How long `record --detach` waits for the background session to start:
// a base, plus a catalog lookup per link.
const (
	detachWait        = 30 * time.Second
	spotifyLookupWait = 15 * time.Second
)

var (
	errCrossfade   = errors.New("spotify crossfade is on, so songs overlap and cannot be cut cleanly")
	errNoAfconvert = errors.New("afconvert (macOS) was not found")
	// errTapHearsNothing is a silent tap while macOS reports the audio
	// permission granted: the sound goes somewhere else.
	errTapHearsNothing = errors.New("spotify is playing, but no sound reaches mixtape (the audio permission is granted)")
)

func newRecordCmd(a *app) *cobra.Command {
	f := &recordFlags{}
	cmd := &cobra.Command{
		Use:   "record [LINK|URI...]",
		Short: "Record tracks, albums and playlists (no links: whatever plays)",
		Long: `Record Spotify tracks, albums or playlists into the library, one tagged M4A
per song. Only what is missing is recorded: songs already in the folder are
skipped, songs elsewhere in the library are copied, and an interrupted run
picks up where it stopped. Partial songs are discarded unless --keep-partial.

Links may be open.spotify.com URLs (any ?si= share link) or spotify: URIs.
Track links go to the Singles folder (or --into). With no links, mixtape
records whatever you play, and presses "next" on songs it already has.

Spotify must be running, and crossfade must be off (Settings → Playback):
crossfaded songs overlap and cannot be cut. Shuffle is turned off for the
session and restored afterwards.

On a terminal: [space] pause/resume, [n] skip the song, [q] quit (Ctrl-C
twice forces it). With -o json (or when piped) the session streams NDJSON
events on stdout: session_started, plan, track_started, track_saved,
track_copied, track_skipped, track_discarded, track_failed, warning and
session_finished.

For agents and scripts: start with --detach, which prints the session ID and
returns at once, then poll 'mixtape wait --timeout 9m' (exit 124 means still
running) or 'mixtape status -o json', and end with 'mixtape stop'.

Exit codes: 0 complete or stopped, 2 usage, 3 not ready, 4 link not found,
5 permission, 6 another session is running, 7 finished with failed songs.`,
		Example: `  mixtape record https://open.spotify.com/playlist/37i9dQZF1DXcBWIGoYBM5M
  mixtape record spotify:album:4LH4d3cOWNNsVw41Gqt2kv --dry-run
  mixtape record spotify:track:4cOdK2wGLETKBW3PvgPWqT --into "Party"
  mixtape record --match "remix"
  mixtape record --detach <link> && mixtape wait --timeout 9m`,
		GroupID: groupRecording,
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.runRecord(cmd, args, f)
		},
	}
	fl := cmd.Flags()
	fl.BoolVar(&f.detach, "detach", false, "Run in the background; print the session ID and return")
	fl.BoolVar(&f.dryRun, "dry-run", false, "Print the plan (record, copy, skip) and change nothing")
	fl.BoolVar(&f.redo, "redo", false, "Record again songs that are already done")
	fl.IntVar(&f.limit, "limit", 0, "Stop after recording this many songs (0 = no limit)")
	fl.StringVar(&f.match, "match", "", "Live mode: record only songs whose title contains this text")
	fl.StringVar(&f.into, "into", "", "Folder for track links and live recordings (default Singles)")
	fl.BoolVar(&f.keepPartial, "keep-partial", false, "Keep songs not captured start to finish (flagged; replaced next run)")
	fl.BoolVar(&f.keepWAV, "keep-wav", false, "Keep each song's WAV next to the M4A work files")
	fl.BoolVar(&f.artwork, "artwork", false, "Embed album artwork (config: artwork)")
	fl.StringVar(&f.capture, "capture", "", "Capture method: auto, tap or device (config: capture)")
	fl.StringVar(&f.device, "device", "", "Capture device name for --capture device (config: device)")
	fl.BoolVar(&f.mute, "mute", false, "Tap only: silence Spotify on the speakers while recording")
	fl.DurationVar(&f.idleTimeout, "idle-timeout", 0, "End after this long with nothing recording (config: idle_timeout)")
	fl.IntVar(&f.maxFailures, "max-failures", config.DefaultMaxFailures,
		"End after this many songs in a row could not be recorded; 0 = never (config: max_failures)")
	fl.BoolVar(&f.allowCrossfade, "allow-crossfade", false, "Record even though Spotify's crossfade is on")
	return cmd
}

// overrides turns the flags that were set into config overrides.
func (f *recordFlags) overrides(cmd *cobra.Command) config.Overrides {
	var o config.Overrides
	fl := cmd.Flags()
	if fl.Changed("capture") {
		o.Capture = &f.capture
	}
	if fl.Changed("device") {
		o.Device = &f.device
	}
	if fl.Changed("artwork") {
		o.Artwork = &f.artwork
	}
	if fl.Changed("idle-timeout") {
		o.IdleTimeout = &f.idleTimeout
	}
	if fl.Changed("max-failures") {
		o.MaxFailures = &f.maxFailures
	}
	return o
}

func (a *app) runRecord(cmd *cobra.Command, args []string, f *recordFlags) error {
	refs, err := parseRefs(cmd, args)
	if err != nil {
		return err
	}
	if f.limit < 0 {
		return &usageError{cmdPath: cmd.CommandPath(), err: fmt.Errorf("%w: --limit must be 0 or more", errInvalidFlag)}
	}
	settings, err := a.settings(f.overrides(cmd))
	if err != nil {
		return err
	}
	if f.detach && a.deps.getenv(session.EnvDetached) == "" {
		if f.dryRun {
			return &usageError{cmdPath: cmd.CommandPath(), err: fmt.Errorf("%w: --detach and --dry-run cannot be combined", errInvalidFlag)}
		}
		return a.detach(cmd.Context(), cmd, args)
	}
	ctx := cmd.Context()

	cols, err := a.resolve(ctx, refs)
	if err != nil {
		return err
	}
	lib, err := library.Open(settings.Library, a.libraryOptions())
	if err != nil {
		return err
	}
	plan := engine.BuildPlan(lib, cols, engine.PlanOptions{Redo: f.redo, Into: f.into})
	if f.dryRun {
		return a.printPlan(plan, settings.Library)
	}
	return a.record(ctx, cmd, lib, plan, settings, f, args)
}

// parseRefs validates every link argument.
func parseRefs(cmd *cobra.Command, args []string) ([]spotify.Ref, error) {
	refs := make([]spotify.Ref, 0, len(args))
	for _, arg := range args {
		ref, err := spotify.ParseRef(arg)
		if err != nil {
			return nil, &usageError{cmdPath: cmd.CommandPath(), err: err}
		}
		if !slices.Contains(refs, ref) {
			refs = append(refs, ref)
		}
	}
	return refs, nil
}

// resolve looks every link up in Spotify's catalog.
func (a *app) resolve(ctx context.Context, refs []spotify.Ref) ([]spotify.Collection, error) {
	cat := a.deps.services.catalog()
	cols := make([]spotify.Collection, 0, len(refs))
	for _, ref := range refs {
		col, err := cat.Resolve(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref.URL(), err)
		}
		switch {
		case col.Source == spotify.SourceOEmbed && ref.Kind != spotify.KindTrack:
			a.out.Warn("%q: the track list is unavailable; mixtape will play it in Spotify and follow it", col.Name)
		case col.Truncated:
			a.out.Warn("%q lists more than %d songs; mixtape will follow the rest in Spotify", col.Name, len(col.Tracks))
		}
		cols = append(cols, col)
	}
	return cols, nil
}

func (a *app) libraryOptions() library.Options {
	cat := a.deps.services.catalog()
	return library.Options{
		Encoder:     a.deps.services.encoder,
		WorkDir:     filepath.Join(a.home, "work"),
		Now:         a.deps.now,
		Warn:        func(msg string) { a.out.Warn("%s", msg) },
		ReleaseDate: cat.ReleaseDate,
	}
}

// planDoc is the JSON shape of `record --dry-run`.
type planDoc struct {
	Mode    string            `json:"mode"`
	Library string            `json:"library"`
	Items   []engine.PlanItem `json:"items"`
	Counts  map[string]int    `json:"counts"`
}

func (a *app) printPlan(plan *engine.Plan, root string) error {
	doc := planDoc{Mode: plan.Mode, Library: root, Items: plan.Summary(), Counts: map[string]int{}}
	if doc.Items == nil {
		doc.Items = []engine.PlanItem{}
	}
	var recordMS int64
	for _, it := range doc.Items {
		doc.Counts[it.Action]++
		if it.Action == string(engine.ActionRecord) {
			recordMS += it.DurationMS
		}
	}
	var b strings.Builder
	if plan.Mode == engine.ModeLive {
		b.WriteString("Live mode: mixtape records whatever Spotify plays into " + plan.Groups[0].Spec.Name)
		return a.out.Result(b.String(), doc)
	}
	fmt.Fprintf(&b, "%d songs: %d to record (%s), %d to copy, %d already done",
		len(doc.Items), doc.Counts["record"], mmss(time.Duration(recordMS)*time.Millisecond),
		doc.Counts["copy"], doc.Counts["skip"])
	if n := doc.Counts["encode"]; n > 0 {
		fmt.Fprintf(&b, ", %d to re-encode", n)
	}
	group := ""
	for _, it := range doc.Items {
		if it.Collection != group {
			group = it.Collection
			fmt.Fprintf(&b, "\n%s", a.out.paint(colorBold, group))
		}
		line := fmt.Sprintf("\n  %-7s %3d  %s", it.Action, it.Position, songLabel(it.Artist, it.Title))
		if it.DurationMS > 0 {
			line += a.out.paint(colorDim, " ("+mmss(time.Duration(it.DurationMS)*time.Millisecond)+")")
		}
		if it.From != "" {
			line += a.out.paint(colorDim, "  ← "+it.From)
		}
		if it.Hint != "" {
			line += a.out.paint(colorYellow, "  ["+it.Hint+"]")
		}
		b.WriteString(line)
	}
	if plan.Mode == engine.ModeFollow {
		b.WriteString("\n" + a.out.paint(colorDim, "…and the rest of the playlist, followed in Spotify"))
	}
	return a.out.Result(b.String(), doc)
}

// record runs a session. When every song is already done (or only needs
// copying or re-encoding) nothing plays, so neither Spotify nor audio
// capture is needed.
func (a *app) record(ctx context.Context, cmd *cobra.Command, lib *library.Library, plan *engine.Plan,
	settings config.Settings, f *recordFlags, args []string,
) error {
	svc := a.deps.services
	playback := needsPlayback(plan)
	if playback {
		if err := a.checkRecordReady(f); err != nil {
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

	p := svc.player()
	pending := a.recoverSpotify(setupCtx, p) // even when nothing plays: a copy-only run must not lose it
	unlock, err := lib.Lock()
	if err != nil {
		return err
	}
	defer unlock()

	rec, err := a.newRecorder(id, plan, settings, nil, args, pending)
	if err != nil {
		return err
	}
	defer rec.close()
	var src capture.Source
	if playback {
		if src, err = a.openSource(setupCtx, cmd, p, settings, f); err != nil {
			return rec.abort(err, stopped())
		}
		rec.setCapture(src)
	}
	if err := engine.Prepare(lib, plan); err != nil {
		closeSource(src)
		return rec.abort(err, false)
	}
	if stopped() {
		closeSource(src)
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
	return rec.run(ctx, lib, plan, p, src, settings, f)
}

// guardSetup handles stop signals while a session sets up: they cancel the
// returned context, and stopped reports whether one arrived. release ends
// the guard (it is safe to call more than once).
func (a *app) guardSetup(ctx context.Context) (setup context.Context, release func(), stopped func() bool) {
	setup, cancel := context.WithCancel(ctx)
	sigs, stopSigs := a.deps.services.signals()
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	var got atomic.Bool
	done := make(chan struct{})
	go func() {
		select {
		case <-done:
		case <-sigs:
			got.Store(true)
			cancel()
		}
	}()
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(done)
			stopSigs()
		})
	}
	return setup, release, got.Load
}

// needsPlayback reports whether the plan records anything.
func needsPlayback(plan *engine.Plan) bool {
	return plan.Mode != engine.ModeQueue || plan.Count(engine.ActionRecord) > 0
}

// openSource checks Spotify answers and opens the capture backend.
func (a *app) openSource(ctx context.Context, cmd *cobra.Command, p player, settings config.Settings,
	f *recordFlags,
) (capture.Source, error) {
	snap := p.Poll(ctx)
	if snap.Err != nil {
		return nil, snap.Err
	}
	if snap.Volume < 100 && !f.mute {
		a.out.Warn("Spotify's volume is %d%%: recordings are made at that level (100%% is best)", snap.Volume)
	}
	method, err := a.selectCapture(settings)
	if err != nil {
		return nil, err
	}
	if f.mute && method != capture.MethodTap {
		return nil, &usageError{cmdPath: cmd.CommandPath(), err: fmt.Errorf("%w: --mute needs --capture tap", errInvalidFlag)}
	}
	if err := a.requireCapturePermission(method); err != nil {
		return nil, err
	}
	return a.deps.services.openCapture(captureRequest{Method: method, Device: settings.Device, Mute: f.mute})
}

// requireCapturePermission fails fast when macOS has not granted the host
// app what the capture method needs: without it the capture is silent.
// An undecided microphone is left alone, because opening the device shows
// the system prompt.
func (a *app) requireCapturePermission(m capture.Method) error {
	k, what := permission.SystemAudio, "record system audio"
	if m == capture.MethodDevice {
		k, what = permission.Microphone, "open the capture device"
	}
	st := a.deps.services.permission(k)
	if st == permission.Denied || (st == permission.Undecided && k == permission.SystemAudio) {
		app := a.deps.services.hostApp()
		return newError(ExitPermission, permission.ErrNotGranted,
			permission.Instructions(k, app)+"; 'mixtape setup' walks you through it",
			"%s may not %s yet, so the recording would be silent", app, what)
	}
	return nil
}

func closeSource(src capture.Source) {
	if src != nil {
		_ = src.Close()
	}
}

// checkRecordReady checks what must hold before anything plays.
func (a *app) checkRecordReady(f *recordFlags) error {
	svc := a.deps.services
	if _, ok := svc.spotifyApp(a.deps.userHome); !ok {
		return spotify.ErrNotInstalled
	}
	if running, err := svc.spotifyRunning(); err == nil && !running {
		return spotify.ErrNotRunning
	}
	if prefs, err := svc.prefs(a.deps.userHome); err == nil && !prefs.Stale &&
		(prefs.Crossfade || prefs.Automix) && !f.allowCrossfade {
		what := fmt.Sprintf("crossfade is set to %.1fs", prefs.CrossfadeSeconds)
		if !prefs.Crossfade {
			what = "automix is on"
		}
		return fmt.Errorf("%w (%s)", errCrossfade, what)
	}
	if _, err := svc.lookPath("afconvert"); err != nil && svc.encoder == nil {
		return errNoAfconvert
	}
	return nil
}

// selectCapture picks the backend: the configured one, else the tap when
// this Mac supports it, else the device.
func (a *app) selectCapture(s config.Settings) (capture.Method, error) {
	m, err := capture.ParseMethod(s.Capture)
	if err != nil {
		return "", err
	}
	return capture.Select(m, a.deps.services.tapStatus())
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
	_ = session.WriteState(a.home, st)
	if len(restored) > 0 {
		a.out.Warn("restored Spotify %s changed by interrupted session %s", strings.Join(restored, ", "), st.ID)
	}
	return nil
}

// detach re-runs this record command in the background and reports how to
// track it.
func (a *app) detach(ctx context.Context, cmd *cobra.Command, args []string) error {
	svc := a.deps.services
	exe, err := svc.executable()
	if err != nil {
		return err
	}
	lockPath, err := svc.lockPath()
	if err != nil {
		return err
	}
	if info, held, _ := session.Inspect(lockPath); held {
		return fmt.Errorf("%w (session %s, pid %d)", session.ErrBusy, info.SessionID, info.PID)
	}
	id := session.NewID(a.deps.now())
	logPath := session.LogPath(a.home, id)
	childArgs := append([]string{"record", "--home", a.home, "-o", formatText, "--no-color"}, childFlags(cmd)...)
	childArgs = append(childArgs, args...)
	proc, err := svc.spawn(exe, childArgs, []string{session.EnvDetached + "=" + id}, logPath)
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(exited)
	}()
	// The child looks every link up before it takes the lock: allow for
	// that, and never leave it running unseen when the wait gives up.
	wctx, cancel := context.WithTimeout(ctx, detachWait+time.Duration(len(args))*spotifyLookupWait)
	defer cancel()
	err = session.AwaitStart(wctx, lockPath, id, func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	})
	if err != nil {
		_ = svc.kill(proc.Pid, syscall.SIGTERM)
		return fmt.Errorf("%w; see %s", err, logPath)
	}
	doc := detachDoc{SessionID: id, PID: proc.Pid, Events: session.EventsPath(a.home, id), Log: logPath}
	text := fmt.Sprintf("recording in the background: session %s (pid %d)\n  follow: mixtape status --follow\n  wait:   mixtape wait\n  stop:   mixtape stop\n  log:    %s",
		id, proc.Pid, logPath)
	return a.out.Result(text, doc)
}

// childFlags forwards the flags given to this record command, minus
// --detach and the global flags the parent sets itself.
func childFlags(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().Visit(func(fl *pflag.Flag) {
		switch fl.Name {
		case "detach", flagOutput, flagNoColor, flagHome, flagQuiet:
			return
		}
		out = append(out, "--"+fl.Name+"="+fl.Value.String())
	})
	return out
}

type detachDoc struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
	Events    string `json:"events"`
	Log       string `json:"log"`
}

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
}

func (a *app) newRecorder(
	id string, plan *engine.Plan, s config.Settings, src capture.Source, args []string, pending *session.SpotifyState,
) (*recorder, error) {
	log, err := session.OpenEventLog(session.EventsPath(a.home, id))
	if err != nil {
		return nil, err
	}
	now := a.deps.now()
	captureName := "none (nothing to play)"
	if src != nil {
		captureName = captureLabel(src)
	}
	r := &recorder{a: a, id: id, log: log, state: session.State{
		ID: id, PID: os.Getpid(), Phase: session.PhaseStarting, Mode: plan.Mode,
		StartedAt: now, UpdatedAt: now, Library: s.Library,
		Capture: captureName, Targets: args,
		Events: log.Path(), Counts: session.Counts{Total: len(plan.Items)},
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

// write saves session.json.
func (r *recorder) write() {
	r.state.UpdatedAt = r.a.deps.now()
	_ = session.WriteState(r.a.home, r.state)
}

// run executes the session and reports how it ended.
func (r *recorder) run(ctx context.Context, lib *library.Library, plan *engine.Plan,
	p player, src capture.Source, s config.Settings, f *recordFlags,
) error {
	a := r.a
	eng, err := engine.New(engine.Config{
		Player: p, Source: src, Library: lib,
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
		return err
	}
	r.eng = eng

	r.emit(engine.Event{
		Type: engine.EventSessionStarted, Mode: plan.Mode, Capture: r.state.Capture,
		Library: s.Library, PID: os.Getpid(),
	})
	r.emit(engine.Event{Type: engine.EventPlan, Items: plan.Summary()})

	runCtx, force := context.WithCancel(context.WithoutCancel(ctx))
	defer force()
	controls := make(chan engine.Control, 8)
	stopSignals := r.watchSignals(controls, force)
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
	_ = r.log.Write(ev)
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

// saveSpotify persists the player settings the session is about to
// change (and later, that they were restored).
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

func (r *recorder) saveSpotify(st session.SpotifyState) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Spotify = &st
	r.state.UpdatedAt = r.a.deps.now()
	return session.WriteState(r.a.home, r.state)
}

// watchSignals turns SIGINT/SIGTERM into a graceful stop, and a second
// SIGINT into a forced quit. SIGHUP and SIGPIPE are ignored so a closed
// terminal or pipe cannot kill a session mid-song.
func (r *recorder) watchSignals(controls chan<- engine.Control, force context.CancelFunc) func() {
	sigs, stop := r.a.deps.services.signals()
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	done := make(chan struct{})
	go func() {
		interrupts := 0
		for {
			select {
			case <-done:
				return
			case sig := <-sigs:
				if sig == os.Interrupt {
					interrupts++
				}
				if interrupts >= 2 {
					force()
					continue
				}
				select {
				case controls <- engine.ControlStop:
				default:
				}
			}
		}
	}()
	return func() {
		close(done)
		stop()
	}
}
