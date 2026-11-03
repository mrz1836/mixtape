package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestRecordClosesTheCaptureWhenTheSessionFailsBeforeCapturing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	src := &nopSource{events: make(chan capture.Event)}
	h.deps.services.openCapture = func(context.Context, captureRequest) (capture.Source, error) { return src, nil }
	// Spotify answers the readiness poll, then quits before the engine
	// sets it up: the session ends before capture ever starts.
	h.world.player.polls = []spotify.Snapshot{
		{State: spotify.StatePaused, Volume: 100},
		{Err: spotify.ErrNotRunning},
	}

	require.Equal(t, ExitNotReady, h.run("record", plURL))
	assert.Positive(t, src.closes.Load(), "the capture source is closed")
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.Equal(t, session.PhaseFinished, st.Phase)
}

func TestRecorderThatCannotStartTheEngineRecordsTheFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := &app{deps: h.deps, out: plainOutput(t, false), home: t.TempDir()}
	lib, err := library.Open(t.TempDir(), library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	run := &recordRun{flags: &recordFlags{}, lib: lib, plan: engine.BuildPlan(lib, nil, engine.PlanOptions{})}
	rec, err := a.newRecorder("s1", run, nil)
	require.NoError(t, err)
	defer rec.close()

	// No player: engine.New refuses the config.
	failure := rec.run(t.Context(), run)
	require.Error(t, failure)
	st, err := session.ReadState(a.home)
	require.NoError(t, err)
	assert.Equal(t, session.PhaseFinished, st.Phase, "the session does not stay starting")
	assert.Equal(t, session.ResultError, st.Result)
}

func TestRecorderWarnsOnceWhenItsStateCannotBeSaved(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	var stderr bytes.Buffer
	out, err := newOutput(outputOptions{format: formatText, stdout: &bytes.Buffer{}, stderr: &stderr})
	require.NoError(t, err)
	// The home is a file, so session.json can never be written.
	home := filepath.Join(t.TempDir(), "home")
	require.NoError(t, os.WriteFile(home, nil, 0o600))
	a := &app{deps: h.deps, out: out, home: home}
	log, err := session.OpenEventLog(filepath.Join(t.TempDir(), "events.ndjson"))
	require.NoError(t, err)
	r := &recorder{a: a, id: "s1", log: log}

	r.write()
	r.write()
	assert.Equal(t, 1, strings.Count(stderr.String(), "could not save the session state"), stderr.String())

	require.NoError(t, log.Close()) // the event log fails from here on
	r.emit(engine.Event{Type: engine.EventWarning, Message: "one"})
	r.emit(engine.Event{Type: engine.EventWarning, Message: "two"})
	assert.Equal(t, 1, strings.Count(stderr.String(), "could not write the session's event log"), stderr.String())
}

func TestTerminalUIIsSilentOnceClosed(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	a := &app{deps: &deps{stdin: strings.NewReader("q")}, out: plainOutput(t, false)}
	u := &ttyUI{a: a, out: out, done: make(chan struct{}), restore: func() {}}
	u.close()
	written := out.String()

	controls := make(chan engine.Control, 1)
	u.readKeys(controls, func() { t.Error("no forced quit after close") })
	u.log("late line")
	u.setStatus("late status")
	assert.Empty(t, controls, "a key after the session ended is not a control")
	assert.Equal(t, written, out.String(), "nothing is printed after the summary")
}

func TestRecordLeavesNoCaptureOpenWhenStoppedDuringSetup(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	src := &nopSource{events: make(chan capture.Event)}
	h.deps.services.openCapture = func(context.Context, captureRequest) (capture.Source, error) { return src, nil }
	h.deps.services.signals = stopAtOnce

	require.Equal(t, ExitOK, h.run("record", plURL))
	assert.Positive(t, src.closes.Load())
}

// stopAtOnce delivers a SIGTERM as soon as signals are watched.
func stopAtOnce() (<-chan os.Signal, func()) {
	ch := make(chan os.Signal, 1)
	ch <- os.Interrupt
	return ch, func() {}
}
