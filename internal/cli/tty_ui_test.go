package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// newTestUI builds the terminal UI around an engine that has not run, so
// its progress is the empty one, writing to out.
func newTestUI(t *testing.T, h *harness, out *syncBuffer) *ttyUI {
	t.Helper()
	lib, err := library.Open(t.TempDir(), library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	eng, err := engine.New(engine.Config{Player: h.world.player, Library: lib})
	require.NoError(t, err)
	a := &app{deps: h.deps, out: plainOutput(t, false), home: t.TempDir()}
	return &ttyUI{a: a, eng: eng, out: out, done: make(chan struct{}), restore: func() {}}
}

func TestStatusLineIsRedrawnUntilTheUICloses(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	out := &syncBuffer{}
	u := newTestUI(t, h, out)
	restored := false
	u.restore = func() { restored = true }

	exited := make(chan struct{})
	go func() {
		u.redraw()
		close(exited)
	}()
	const idle = "\r\x1b[K⏸ WAIT │ waiting for Spotify… │ 0 recorded"
	require.Eventually(t, func() bool { return strings.Contains(out.String(), idle) }, 5*time.Second, 10*time.Millisecond,
		"the status line is drawn in place, from the engine's progress")

	u.close()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		require.Fail(t, "the redraw loop ends with the UI")
	}
	assert.True(t, restored, "the terminal leaves raw mode")
	assert.True(t, strings.HasSuffix(out.String(), "\r\x1b[K"), "the status line is cleared for the summary")
	u.close() // idempotent
}

func TestLogLinesGoAboveTheStatusLine(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	out := &syncBuffer{}
	u := newTestUI(t, h, out)

	u.setStatus("⏺ REC 0:01/3:00")
	u.log("✓ saved Song\nsecond line")
	assert.Equal(t, "\r\x1b[K⏺ REC 0:01/3:00"+"\r\x1b[K✓ saved Song\r\nsecond line\r\n⏺ REC 0:01/3:00", out.String(),
		"raw mode needs CRLF, and the status line comes back below the log")
}

func TestUIWidthFallsBackWithoutATerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	u := newTestUI(t, h, &syncBuffer{})
	assert.Equal(t, 100, u.width(), "no stdin at all")

	f, err := os.CreateTemp(t.TempDir(), "stdin")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	h.deps.stdinFile = f
	assert.Equal(t, 100, u.width(), "a stdin that is not a terminal has no size")
}

func TestTerminalUINeedsARealTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	h.deps.stdinFile = f
	a := &app{deps: h.deps, out: plainOutput(t, false)}

	u, err := startTTY(a, nil, make(chan engine.Control), func() {})
	require.Error(t, err)
	assert.Nil(t, u)
	assert.Empty(t, h.stdout.String(), "nothing is drawn")
}

func TestRecordWithAStdinThatCannotGoRawPrintsPlainLines(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.interactive()
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	h.deps.stdinFile = f
	h.world.addPlaylist("Done List", songs()...)
	h.seedLibrary(playlistSpec("Done List"), songs()...)

	require.Equal(t, ExitOK, h.run("record", plURL, "--no-color"), h.stderr.String())
	out := h.stdout.String()
	assert.NotContains(t, out, "\r", "no status line without raw mode")
	assert.Contains(t, out, "plan: 0 to record, 0 to copy, 2 already done\n")
	assert.Contains(t, out, "complete: 0 recorded, 0 copied, 2 skipped\n")
}

func TestRecorderWritesAboveTheStatusLineWhileTheUIRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	out := &syncBuffer{}
	u := newTestUI(t, h, out)
	log, err := session.OpenEventLog(filepath.Join(t.TempDir(), "s1.ndjson"))
	require.NoError(t, err)
	r := &recorder{a: u.a, id: "s1", log: log, ui: u}

	r.warn("disk %s", "nearly full")
	r.emit(engine.Event{Type: engine.EventTrackStarted, Track: &engine.EventTrack{Title: "Song", Artist: "Band", DurationMS: 64333}})
	assert.Contains(t, out.String(), "\r\x1b[K! disk nearly full\r\n")
	assert.Contains(t, out.String(), "\r\x1b[K▶ Band — Song (1:04)\r\n")
	assert.Empty(t, h.stderr.String(), "warnings stay off stderr, under the status line")

	r.close()
	before := out.String()
	r.emit(engine.Event{Type: engine.EventWarning, Message: "late"})
	assert.Equal(t, before, out.String(), "nothing is written once the recorder is closed")
	data, err := os.ReadFile(log.Path())
	require.NoError(t, err)
	assert.Equal(t, 1, strings.Count(string(data), "\n"), "the event log stops with it")
}

func TestStopSignals(t *testing.T) {
	t.Parallel()
	controls := make(chan engine.Control, 1)
	forced := 0
	onSignal := stopOnSignal(controls, func() { forced++ })

	onSignal(syscall.SIGTERM)
	onSignal(syscall.SIGTERM) // the request is already queued: not a second one, and no block
	assert.Equal(t, engine.ControlStop, <-controls, "mixtape stop is a graceful stop")
	assert.Empty(t, controls)
	assert.Zero(t, forced, "SIGTERM never forces the quit")

	onSignal(os.Interrupt)
	assert.Equal(t, engine.ControlStop, <-controls, "the first Ctrl-C stops gracefully")
	onSignal(os.Interrupt)
	assert.Equal(t, 1, forced, "the second Ctrl-C forces it")
	assert.Empty(t, controls)
}

func TestSessionSavesSpotifySettingsBeforeChangingThem(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := &app{deps: h.deps, out: plainOutput(t, false), home: t.TempDir()}
	lib, err := library.Open(t.TempDir(), library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	run := &recordRun{flags: &recordFlags{}, lib: lib, plan: engine.BuildPlan(lib, nil, engine.PlanOptions{})}
	on, off := true, false
	pending := &session.SpotifyState{Original: session.PlayerSettings{Shuffle: &on}, Set: session.PlayerSettings{Shuffle: &off}}
	r, err := a.newRecorder("s1", run, pending)
	require.NoError(t, err)
	defer r.close()

	got := r.pendingSpotify()
	require.NotNil(t, got, "what the interrupted session left is handed to the engine")
	assert.Equal(t, *pending, *got)
	assert.NotSame(t, pending, got, "the engine gets its own copy")

	saved := session.SpotifyState{Original: session.PlayerSettings{Repeat: &off}, Set: session.PlayerSettings{Repeat: &on}}
	require.NoError(t, r.saveSpotify(saved))
	st, err := session.ReadState(a.home)
	require.NoError(t, err)
	require.NotNil(t, st.Spotify, "a crash from here on can still put them back")
	assert.Equal(t, saved, *st.Spotify)

	saved.Restored = true
	require.NoError(t, r.saveSpotify(saved))
	assert.Nil(t, r.pendingSpotify(), "nothing is left to put back")
}

func TestMoreEventLines(t *testing.T) {
	t.Parallel()
	o := plainOutput(t, false)
	assert.Equal(t, "mixtape: queue session, capture tap (Spotify), library /Music",
		eventLine(engine.Event{Type: engine.EventSessionStarted, Mode: engine.ModeQueue, Capture: "tap (Spotify)", Library: "/Music"}, o))
	assert.Equal(t, "plan: 1 to record, 0 to copy, 0 already done",
		eventLine(engine.Event{Type: engine.EventPlan, Items: []engine.PlanItem{{Action: "record"}}}, o))
	assert.Empty(t, eventLine(engine.Event{Type: engine.EventSessionFinished}, o), "the summary line reports the end")

	offset := -0.125
	b := engine.Event{Type: engine.EventBoundary, Track: &engine.EventTrack{Title: "Song"}, OffsetSeconds: &offset, Runs: 2, Reason: "end"}
	assert.Equal(t, "  boundary Song: offset -0.125s, drift 0.000s, runs 2 end", eventLine(b, plainOutput(t, true)),
		"a boundary without a measured drift reads 0")
}

func TestStatusLineRoundsAShortETAUp(t *testing.T) {
	t.Parallel()
	p := engine.Progress{
		Mode: engine.ModeQueue, State: spotify.StatePlaying,
		Counts: session.Counts{Total: 2, Recorded: 1}, ETA: 30 * time.Second,
	}
	assert.Contains(t, statusLine(p, plainOutput(t, false), 200), "1 of 2 · ~1m left", "never ~0m")
	assert.Contains(t, statusLine(p, plainOutput(t, false), 200), "SKIP", "playing but not recording")
}
