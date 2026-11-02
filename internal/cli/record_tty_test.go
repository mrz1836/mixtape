package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// plainOutput is a text Output without color.
func plainOutput(t *testing.T, verbose bool) *Output {
	t.Helper()
	o, err := newOutput(outputOptions{format: formatText, verbose: verbose, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}})
	require.NoError(t, err)
	return o
}

func TestKeysBecomeControls(t *testing.T) {
	t.Parallel()
	out := &bytes.Buffer{}
	a := &app{deps: &deps{stdin: strings.NewReader(" xnQ\x03\x03")}, out: plainOutput(t, false)}
	u := &ttyUI{a: a, out: out, done: make(chan struct{}), restore: func() {}}
	controls := make(chan engine.Control, 8)
	forced := 0

	u.readKeys(controls, func() { forced++ }) // returns at the end of input
	close(controls)
	var got []engine.Control
	for c := range controls {
		got = append(got, c)
	}
	assert.Equal(t, []engine.Control{
		engine.ControlPlayPause, engine.ControlNext, engine.ControlStop, engine.ControlStop,
	}, got, "space, n, Q and the first Ctrl-C; other keys are ignored")
	assert.Equal(t, 1, forced, "the second Ctrl-C forces the quit")
	assert.Contains(t, out.String(), "Ctrl-C again to force")
}

func TestStatusLine(t *testing.T) {
	t.Parallel()
	o := plainOutput(t, false)
	p := engine.Progress{
		Mode: engine.ModeQueue, State: spotify.StatePlaying, Recording: true,
		Track:    &engine.EventTrack{Title: "Still Loving You", Artist: "Scorpions"},
		Position: 72, Duration: 225,
		Counts: session.Counts{Total: 12, Recorded: 2, Skipped: 1}, ETA: 32 * time.Minute,
	}
	line := statusLine(p, o, 200)
	assert.Contains(t, line, "REC")
	assert.Contains(t, line, "1:12/3:45")
	assert.Contains(t, line, "Scorpions — Still Loving You")
	assert.Contains(t, line, "3 of 12")
	assert.Contains(t, line, "left")

	p.State, p.Recording = spotify.StatePaused, false
	assert.Contains(t, statusLine(p, o, 200), "WAIT")
	p.Phase = "stopping"
	assert.Contains(t, statusLine(p, o, 200), "STOP")
	p = engine.Progress{Mode: engine.ModeLive, State: spotify.StatePlaying}
	assert.Contains(t, statusLine(p, o, 200), "waiting for Spotify")
	assert.Contains(t, statusLine(p, o, 200), "0 recorded", "live mode counts recordings")

	long := engine.Progress{Track: &engine.EventTrack{Title: strings.Repeat("Very Long Title ", 20)}}
	assert.LessOrEqual(t, len([]rune(statusLine(long, o, 40))), 40, "the line fits the terminal")
}

func TestEventLines(t *testing.T) {
	t.Parallel()
	o := plainOutput(t, false)
	tr := &engine.EventTrack{Title: "Song", Artist: "Band", DurationMS: 64333, File: "Singles/Band-Song.m4a"}
	cases := map[string]string{
		engine.EventTrackStarted:   "▶ Band — Song (1:04)",
		engine.EventTrackSaved:     "✓ saved Band — Song → Singles/Band-Song.m4a",
		engine.EventTrackCopied:    "⧉ copied Band — Song → Singles/Band-Song.m4a",
		engine.EventTrackSkipped:   "· skipped Band — Song (why)",
		engine.EventTrackDiscarded: "✗ discarded Band — Song: why",
		engine.EventTrackFailed:    "✗ failed Band — Song: why",
	}
	for typ, want := range cases {
		assert.Equal(t, want, eventLine(engine.Event{Type: typ, Track: tr, Reason: "why"}, o), typ)
	}
	partial := *tr
	partial.Partial = true
	assert.Contains(t, eventLine(engine.Event{Type: engine.EventTrackSaved, Track: &partial}, o), "partial")
	assert.Equal(t, "! careful", eventLine(engine.Event{Type: engine.EventWarning, Message: "careful"}, o))

	offset := 0.25
	b := engine.Event{Type: engine.EventBoundary, Track: tr, OffsetSeconds: &offset}
	assert.Empty(t, eventLine(b, o), "boundaries only in verbose mode")
	assert.Contains(t, eventLine(b, plainOutput(t, true)), "offset +0.250s")
}

func TestPlanAndSummaryLines(t *testing.T) {
	t.Parallel()
	o := plainOutput(t, false)
	items := []engine.PlanItem{{Action: "record"}, {Action: "record"}, {Action: "copy"}, {Action: "skip"}}
	assert.Equal(t, "plan: 2 to record, 1 to copy, 1 already done", planLine(items))
	assert.Equal(t, "recording whatever Spotify plays", planLine(nil))

	res := engine.Result{Outcome: engine.OutcomeIncomplete, Counts: session.Counts{Recorded: 3, Failed: 1, Remaining: 2}}
	line := summaryLine(res, o)
	assert.Contains(t, line, "incomplete: 3 recorded")
	assert.Contains(t, line, "1 failed")
	assert.Contains(t, line, "2 remaining")
}

func TestOutcomeExit(t *testing.T) {
	t.Parallel()
	exit, err := outcomeExit(engine.Result{Outcome: engine.OutcomeComplete}, false)
	assert.Equal(t, ExitOK, exit)
	require.NoError(t, err)

	exit, _ = outcomeExit(engine.Result{Outcome: engine.OutcomeStopped}, false)
	assert.Equal(t, ExitOK, exit, "a stop on request is a success")

	exit, err = outcomeExit(engine.Result{Outcome: engine.OutcomeIncomplete, Counts: session.Counts{Failed: 1, Remaining: 2}}, false)
	assert.Equal(t, ExitIncomplete, exit)
	assert.Contains(t, err.Error(), "3 song(s) not recorded")

	exit, _ = outcomeExit(engine.Result{Outcome: engine.OutcomeComplete}, true)
	assert.Equal(t, ExitInterrupted, exit, "a forced quit")

	exit, err = outcomeExit(engine.Result{Outcome: engine.OutcomeIncomplete, Err: engine.ErrNoSound}, false)
	assert.Equal(t, ExitNotReady, exit)
	var e *Error
	require.ErrorAs(t, err, &e)
	assert.Contains(t, e.Hint, "Spotify Connect")
}

func TestSilentTapWithThePermissionPointsElsewhere(t *testing.T) {
	t.Parallel()
	e := mapErr(errTapHearsNothing)
	assert.Equal(t, ExitNotReady, e.Exit)
	assert.Contains(t, e.Hint, "another device")
	assert.NotContains(t, e.Hint, "System Audio Recording", "the permission is already granted")
}
