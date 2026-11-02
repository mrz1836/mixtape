package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	selfupdate "github.com/mrz1836/go-selfupdate"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/capture/tap"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

const (
	plID  = "37i9dQZF1DXcBWIGoYBM5M"
	trkA  = "4cOdK2wGLETKBW3PvgPWqT"
	trkB  = "7gK6ZSHZVnigjKdmsXE8Hi"
	plURL = "https://open.spotify.com/playlist/" + plID + "?si=abc"
)

func songs() []spotify.CatalogTrack {
	return []spotify.CatalogTrack{
		{ID: trkA, Title: "Never Gonna Give You Up", Artist: "Rick Astley", Duration: 213 * time.Second},
		{ID: trkB, Title: "Second", Artist: "Someone", Duration: 180 * time.Second},
	}
}

// ndjson decodes stdout as an event stream.
func (h *harness) ndjson() []engine.Event {
	h.t.Helper()
	var out []engine.Event
	sc := bufio.NewScanner(bytes.NewReader(h.stdout.Bytes()))
	for sc.Scan() {
		var ev engine.Event
		require.NoError(h.t, json.Unmarshal(sc.Bytes(), &ev), "line: %s", sc.Text())
		out = append(out, ev)
	}
	return out
}

func TestRecordDryRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("Today's Top Hits", songs()...)

	require.Equal(t, ExitOK, h.run("record", plURL, "--dry-run"))
	doc := h.json()
	assert.Equal(t, "queue", doc["mode"])
	items := doc["items"].([]any)
	require.Len(t, items, 2)
	first := items[0].(map[string]any)
	assert.Equal(t, "record", first["action"])
	assert.Equal(t, trkA, first["id"])
	assert.Equal(t, "Today's Top Hits", first["collection"])
	assert.InDelta(t, 2, doc["counts"].(map[string]any)["record"], 0)
	assert.Empty(t, h.world.player.Commands(), "a dry run never touches Spotify")

	entries, err := os.ReadDir(h.env["MIXTAPE_LIBRARY"])
	require.NoError(t, err)
	assert.Empty(t, entries, "a dry run creates nothing")

	require.Equal(t, ExitOK, h.run("record", plURL, "--dry-run", "-o", "text"))
	assert.Contains(t, h.stdout.String(), "2 songs: 2 to record (6:33), 0 to copy, 0 already done")
	assert.Contains(t, h.stdout.String(), "Rick Astley — Never Gonna Give You Up")
}

func TestRecordUsageErrors(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"not a link":         {"record", "hello"},
		"episode link":       {"record", "https://open.spotify.com/episode/4cOdK2wGLETKBW3PvgPWqT"},
		"negative limit":     {"record", "--limit", "-1"},
		"detach and dry-run": {"record", "--detach", "--dry-run", plURL},
		"bad capture flag":   {"record", "--capture", "magic", plURL},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			assert.Equal(t, ExitUsage, h.run(args...))
			assert.Equal(t, "usage", h.errDoc()["code"])
		})
	}
}

func TestRecordNotFoundLink(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitNotFound, h.run("record", plURL))
	e := h.errDoc()
	assert.Equal(t, "not_found", e["code"])
	assert.Contains(t, e["message"], plID)
}

func TestRecordNotReady(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup func(w *fakeWorld)
		want  string
	}{
		"crossfade on":      {func(w *fakeWorld) { w.prefs.Crossfade, w.prefs.CrossfadeSeconds = true, 6.658 }, "Crossfade songs"},
		"spotify stopped":   {func(w *fakeWorld) { w.running = false }, "open -a Spotify"},
		"spotify missing":   {func(w *fakeWorld) { w.installed = false }, "spotify.com/download"},
		"tap but not mac14": {func(w *fakeWorld) { w.tap = capture.TapStatus{Reason: "old macOS"} }, ""},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.world.addPlaylist("List", songs()...)
			tc.setup(h.world)
			if name == "tap but not mac14" {
				h.env["MIXTAPE_CAPTURE"] = "tap"
			}
			assert.Equal(t, ExitNotReady, h.run("record", plURL))
			e := h.errDoc()
			assert.Equal(t, "not_ready", e["code"])
			if tc.want != "" {
				assert.Contains(t, e["hint"], tc.want)
			}
		})
	}
}

func TestRecordAllowCrossfadeGetsPastTheCheck(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.prefs.Crossfade = true
	require.NoError(t, (&app{deps: h.deps}).checkRecordReady(&recordFlags{allowCrossfade: true}))
	require.ErrorIs(t, (&app{deps: h.deps}).checkRecordReady(&recordFlags{}), errCrossfade)
}

func TestRecordBusy(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 999, SessionID: "other"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	require.Equal(t, ExitBusy, h.run("record", plURL))
	assert.Equal(t, "busy", h.errDoc()["code"])
}

func TestRecordEverythingDoneNeedsNoPlayback(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ref := h.world.addPlaylist("Done List", songs()...)
	spec := library.Spec{Kind: library.KindPlaylist, SpotifyID: ref.ID, Name: "Done List"}
	for _, s := range songs() {
		recordDone(t, h.env["MIXTAPE_LIBRARY"], spec, library.Track{ID: s.ID, Title: s.Title, Artist: s.Artist, DurationMS: s.Duration.Milliseconds()})
	}
	h.world.running = false // not even needed

	require.Equal(t, ExitOK, h.run("record", plURL))
	evs := h.ndjson()
	types := make([]string, 0, len(evs))
	for _, ev := range evs {
		types = append(types, ev.Type)
		assert.NotEmpty(t, ev.SessionID)
	}
	assert.Equal(t, []string{"session_started", "plan", "track_skipped", "track_skipped", "session_finished"}, types)
	last := evs[len(evs)-1]
	assert.Equal(t, "complete", last.Result)
	require.NotNil(t, last.ExitCode)
	assert.Equal(t, 0, *last.ExitCode)
	assert.Empty(t, h.world.player.Commands())

	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.Equal(t, session.PhaseFinished, st.Phase)
	assert.Equal(t, 2, st.Counts.Skipped)
	assert.FileExists(t, st.Events)

	// The log holds the same events, compact.
	data, err := os.ReadFile(st.Events)
	require.NoError(t, err)
	assert.Equal(t, 5, strings.Count(string(data), "\n"))
}

func TestStatusWaitStop(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]

	require.Equal(t, ExitNotFound, h.run("status"))
	assert.Equal(t, "not_found", h.errDoc()["code"])
	require.Equal(t, ExitNotFound, h.run("stop"))

	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: "s1", Home: home})
	require.NoError(t, err)
	running := session.State{
		ID: "s1", PID: 4242, Phase: session.PhaseRecording, StartedAt: h.now(),
		Counts: session.Counts{Total: 3, Recorded: 1, Remaining: 2}, ETASeconds: 400,
		Current: &session.Current{Title: "Song", Artist: "Band", Position: 30, Duration: 200, Recording: true},
	}
	require.NoError(t, session.WriteState(home, running))

	require.Equal(t, ExitOK, h.run("status"))
	doc := h.json()
	assert.Equal(t, true, doc["running"])
	assert.Equal(t, "s1", doc["session"].(map[string]any)["session_id"])

	h.deps.stdoutTTY = true
	require.Equal(t, ExitOK, h.run("status"))
	assert.Contains(t, h.stdout.String(), "now:     Band — Song  0:30 / 3:20")
	h.deps.stdoutTTY = false

	require.Equal(t, ExitTimeout, h.run("wait", "--timeout", "2s"))
	assert.Equal(t, "timeout", h.errDoc()["code"])

	// stop: the recorder exits on SIGTERM and records its end.
	h.world.onKill = func(int) {
		code := 0
		running.Phase, running.Result, running.ExitCode = session.PhaseFinished, session.ResultStopped, &code
		require.NoError(t, session.WriteState(home, running))
		require.NoError(t, lock.Release())
	}
	require.Equal(t, ExitOK, h.run("stop"))
	assert.Equal(t, []int{4242}, h.world.killed)
	assert.Equal(t, "stopped", h.json()["session"].(map[string]any)["result"])

	require.Equal(t, ExitOK, h.run("wait"), "a finished session returns its own result at once")
	require.Equal(t, ExitOK, h.run("status"))
	assert.Equal(t, false, h.json()["running"])
}

func TestStopRacingTheSessionsEndReportsItsResult(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: "s1", Home: home})
	require.NoError(t, err)
	st := session.State{ID: "s1", PID: 4242, Phase: session.PhaseRecording, StartedAt: h.now()}
	require.NoError(t, session.WriteState(home, st))

	// The session finishes on its own just before the signal arrives.
	h.world.onKill = func(int) {
		code := 0
		st.Phase, st.Result, st.ExitCode = session.PhaseFinished, session.ResultComplete, &code
		require.NoError(t, session.WriteState(home, st))
		require.NoError(t, lock.Release())
	}
	h.world.killErr = syscall.ESRCH
	require.Equal(t, ExitOK, h.run("stop", "--no-wait"), "a session that already ended is stopped")
	assert.Equal(t, "complete", h.json()["session"].(map[string]any)["result"])
}

func TestStatusFollowStreamsTheEventsUntilTheSessionEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: "s1", Home: home})
	require.NoError(t, err)
	log, err := session.OpenEventLog(session.EventsPath(home, "s1"))
	require.NoError(t, err)
	code := 0
	for _, ev := range []engine.Event{
		{Type: engine.EventTrackStarted, Track: &engine.EventTrack{Title: "Song", Artist: "Band", DurationMS: 64333}},
		{Type: engine.EventTrackSaved, Track: &engine.EventTrack{Title: "Song", Artist: "Band", File: "Singles/Band-Song.m4a"}},
		{Type: engine.EventSessionFinished, Result: engine.OutcomeComplete, Counts: &session.Counts{Total: 1, Recorded: 1}, ExitCode: &code},
	} {
		require.NoError(t, log.Write(ev))
	}
	require.NoError(t, log.Close())
	require.NoError(t, session.WriteState(home, session.State{
		ID: "s1", PID: 4242, Phase: session.PhaseRecording, StartedAt: h.now(), Events: session.EventsPath(home, "s1"),
	}))
	// The session ends while status follows it.
	h.world.onSleep = func() { _ = lock.Release() }

	h.deps.stdoutTTY = true
	require.Equal(t, ExitOK, h.run("status", "--follow", "--no-color"))
	out := h.stdout.String()
	assert.Contains(t, out, "▶ Band — Song (1:04)")
	assert.Contains(t, out, "✓ saved Band — Song → Singles/Band-Song.m4a")
	assert.Contains(t, out, "complete: 1 recorded")
}

func TestCrashLeftSpotifySettingsSurviveARunThatCannotRestoreThem(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	ref := h.world.addPlaylist("Done List", songs()...)
	spec := library.Spec{Kind: library.KindPlaylist, SpotifyID: ref.ID, Name: "Done List"}
	for _, s := range songs() {
		recordDone(t, h.env["MIXTAPE_LIBRARY"], spec, library.Track{ID: s.ID, Title: s.Title, Artist: s.Artist, DurationMS: s.Duration.Milliseconds()})
	}
	vol, set := 47, 100
	crashed := session.State{ID: "old", Phase: session.PhaseRecording, Spotify: &session.SpotifyState{
		Original: session.PlayerSettings{Volume: &vol}, Set: session.PlayerSettings{Volume: &set},
	}}
	require.NoError(t, session.WriteState(home, crashed))
	h.world.running = false // Spotify is closed: nothing can be put back now
	h.world.player.snap = spotify.Snapshot{Err: spotify.ErrNotRunning}

	require.Equal(t, ExitOK, h.run("record", plURL), "a copy-only run needs no Spotify")
	st, err := session.ReadState(home)
	require.NoError(t, err)
	assert.NotEqual(t, "old", st.ID)
	require.NotNil(t, st.Spotify, "the unrestored settings are carried into the new session")
	assert.True(t, st.Spotify.NeedsRestore())
	assert.Equal(t, 47, *st.Spotify.Original.Volume)
}

func TestChildFlagsForwardRecordFlagsOnly(t *testing.T) {
	t.Parallel()
	a := &app{}
	cmd := newRecordCmd(a)
	require.NoError(t, cmd.ParseFlags([]string{"--detach", "--mute", "--limit", "3", "--max-failures", "5"}))
	got := childFlags(cmd)
	assert.ElementsMatch(t, []string{"--mute=true", "--limit=3", "--max-failures=5"}, got)
}

func TestWaitReturnsTheSessionExitCode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	code := ExitIncomplete
	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{
		ID: "s2", Phase: session.PhaseFinished, Result: session.ResultIncomplete, ExitCode: &code,
	}))
	require.Equal(t, ExitIncomplete, h.run("wait"))

	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{ID: "s3", Phase: session.PhaseRecording}))
	require.Equal(t, ExitError, h.run("wait"), "a crashed session is an error")
	require.Equal(t, ExitOK, h.run("status"))
	assert.Equal(t, true, h.json()["stale"])
}

func TestDoctor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("doctor"))
	doc := h.json()
	assert.Equal(t, true, doc["ready"])
	assert.Equal(t, "tap", doc["capture"])

	h.world.prefs.Crossfade, h.world.prefs.CrossfadeSeconds = true, 6.658
	h.world.player.snap.Volume = 48
	require.Equal(t, ExitNotReady, h.run("doctor"))
	checks := map[string]map[string]any{}
	for _, raw := range h.json()["checks"].([]any) {
		c := raw.(map[string]any)
		checks[c["id"].(string)] = c
	}
	assert.Equal(t, "fail", checks["crossfade"]["status"])
	assert.Contains(t, checks["crossfade"]["detail"], "6.7s")
	assert.Equal(t, "warn", checks["volume"]["status"])
	assert.Equal(t, "ok", checks["library"]["status"])
}

func TestDoctorDeviceRouting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["MIXTAPE_CAPTURE"] = "device"
	require.Equal(t, ExitNotReady, h.run("doctor"), "the speakers are the output: BlackHole hears nothing")

	h.world.output = device.Info{Name: "Multi-Output Device", Output: true, DefaultOutput: true}
	require.Equal(t, ExitOK, h.run("doctor"))

	h.world.devices = nil
	require.Equal(t, ExitNotReady, h.run("doctor"))
	h.deps.stdoutTTY = true
	require.Equal(t, ExitNotReady, h.run("doctor"))
	assert.Contains(t, h.stdout.String(), "brew install blackhole-2ch")
}

func TestListAndVerify(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("list"))
	assert.Empty(t, h.json()["collections"])

	ref := h.world.addPlaylist("Mix", songs()...)
	spec := library.Spec{Kind: library.KindPlaylist, SpotifyID: ref.ID, Name: "Mix"}
	recordDone(t, h.env["MIXTAPE_LIBRARY"], spec, library.Track{ID: trkA, Title: "Never Gonna Give You Up", Artist: "Rick Astley"})

	require.Equal(t, ExitOK, h.run("ls"))
	cols := h.json()["collections"].([]any)
	require.Len(t, cols, 1)
	assert.Equal(t, "Mix", cols[0].(map[string]any)["name"])

	require.Equal(t, ExitOK, h.run("list", plURL))
	tracks := h.json()["tracks"].([]any)
	require.Len(t, tracks, 2)
	assert.Equal(t, "recorded", tracks[0].(map[string]any)["status"])
	assert.Equal(t, "skip", tracks[0].(map[string]any)["next_action"])
	assert.Equal(t, "new", tracks[1].(map[string]any)["status"])
	assert.Equal(t, "record", tracks[1].(map[string]any)["next_action"])

	require.Equal(t, ExitOK, h.run("verify"))
	assert.InDelta(t, 1, h.json()["ok"], 0)
}

func TestDevicesAndSetupWithoutTerminal(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("devices"))
	doc := h.json()
	assert.Equal(t, "MacBook Pro Speakers", doc["default_output"])
	assert.Len(t, doc["devices"], 2)

	require.Equal(t, ExitOK, h.run("setup"))
	assert.Equal(t, true, h.json()["ready"])
	h.world.running = false
	require.Equal(t, ExitNotReady, h.run("setup", "-o", "text"))
	assert.Contains(t, h.stdout.String(), "Manual setup:")
}

func TestExitCodeTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		err  error
		exit int
	}{
		{nil, ExitOK},
		{errors.New("boom"), ExitError}, //nolint:err113 // an arbitrary error
		{spotify.ErrBadLink, ExitUsage},
		{config.ErrInvalidValue, ExitUsage},
		{errCrossfade, ExitNotReady},
		{spotify.ErrNotRunning, ExitNotReady},
		{device.ErrNoDevice, ExitNotReady},
		{engine.ErrSilentDevice, ExitNotReady},
		{spotify.ErrNotFound, ExitNotFound},
		{session.ErrNoSession, ExitNotFound},
		{spotify.ErrNotAuthorized, ExitPermission},
		{engine.ErrSilentTap, ExitPermission},
		{tap.ErrCreateTap, ExitPermission},
		{selfupdate.ErrInstallDirNotWritable, ExitPermission},
		{selfupdate.ErrManagedInstall, ExitPermission},
		{os.ErrPermission, ExitPermission},
		{session.ErrBusy, ExitBusy},
		{library.ErrLocked, ExitBusy},
		{errTimeout, ExitTimeout},
		{fmt.Errorf("wrapped: %w", spotify.ErrNotRunning), ExitNotReady},
	}
	for _, tc := range tests {
		got := ExitOK
		if e := mapErr(tc.err); e != nil {
			got = e.Exit
			assert.Equal(t, codeFor(e.Exit), e.Code)
		}
		assert.Equal(t, tc.exit, got, "%v", tc.err)
	}
}

func TestDoctorLibraryCheckCreatesNothing(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "not", "yet")
	c := checkLibrary(root)
	assert.Equal(t, checkOK, c.Status)
	assert.Contains(t, c.Detail, "created on first use")
	assert.NoDirExists(t, root)

	file := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	assert.Equal(t, checkFail, checkLibrary(file).Status)
}

func TestHumanDurations(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "just now", ago(20*time.Second))
	assert.Equal(t, "5m ago", ago(5*time.Minute))
	assert.Equal(t, "1h05m", roundETA(65*time.Minute))
	assert.Equal(t, "3:12", mmss(192*time.Second))
	assert.Equal(t, "1:02:03", mmss(time.Hour+2*time.Minute+3*time.Second))
	assert.Equal(t, "ab…", truncateDisplay("abcdef", 3))
	assert.Equal(t, "\x1b[31mab\x1b[0m", truncateDisplay("\x1b[31mab\x1b[0m", 3), "color codes do not count")
}

func TestSetupInteractiveVerifiesAndSavesTheTap(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.deps.stdin = strings.NewReader("y\n\n~/Tapes\n")
	h.deps.services.openCapture = func(r captureRequest) (capture.Source, error) {
		assert.Equal(t, capture.MethodTap, r.Method)
		return &loudSource{}, nil
	}

	require.Equal(t, ExitOK, h.run("setup"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "heard audio")
	assert.Contains(t, out, "saved capture = tap")
	assert.Contains(t, out, "ready to record (tap capture)")

	f, _, err := config.Load(filepath.Join(h.env["MIXTAPE_HOME"], "config.json"))
	require.NoError(t, err)
	assert.Equal(t, "tap", *f.Capture)
	assert.Equal(t, "~/Tapes", *f.Library)
}

func TestSetupFallsBackToBlackHoleWhenTheTapIsSilent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.world.output = device.Info{Name: "Multi-Output Device", Output: true, DefaultOutput: true}
	h.world.player.snap.State = spotify.StatePlaying // Spotify plays; the tap still hears nothing
	h.deps.services.openCapture = func(r captureRequest) (capture.Source, error) {
		if r.Method == capture.MethodTap {
			return &nopSource{}, nil // a tap without permission hears nothing
		}
		return &loudSource{}, nil
	}

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "the tap test failed")
	assert.Contains(t, out, "Screen & System Audio Recording")
	assert.Contains(t, out, "saved capture = device")
}

func TestDoctorNamesTheMissingSystemAudioPermission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.perms[permission.SystemAudio] = permission.Undecided
	require.Equal(t, ExitNotReady, h.run("doctor"))
	var found map[string]any
	for _, raw := range h.json()["checks"].([]any) {
		if c := raw.(map[string]any); c["id"] == string(permission.SystemAudio) {
			found = c
		}
	}
	require.NotNil(t, found, "the tap method reports its permission")
	assert.Equal(t, "fail", found["status"])
	assert.Contains(t, found["detail"], "iTerm has not been allowed")
	assert.Contains(t, found["detail"], "on in the screen-recording list, which does not cover the tap")
	assert.Contains(t, found["fix"], "add iTerm under")
	assert.Contains(t, found["fix"], "System Audio Recording Only")
}

func TestDoctorNeverPromptsForAutomation(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.perms[permission.Automation] = permission.Undecided
	require.Equal(t, ExitOK, h.run("doctor"), "an undecided prompt is a warning, not a failure")
	for _, raw := range h.json()["checks"].([]any) {
		if c := raw.(map[string]any); c["id"] == "automation" {
			assert.Equal(t, "warn", c["status"])
			assert.Contains(t, c["fix"], "mixtape setup")
		}
	}
	assert.Empty(t, h.world.player.Commands(), "doctor did not poll Spotify, which would prompt")
}

func TestRecordStopsAtOnceWithoutTheTapPermission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.world.perms[permission.SystemAudio] = permission.Undecided

	require.Equal(t, ExitPermission, h.run("record", plURL))
	e := h.errDoc()
	assert.Equal(t, "permission", e["code"])
	assert.Contains(t, e["message"], "iTerm may not record system audio yet")
	assert.Contains(t, e["hint"], "System Audio Recording Only")
	assert.Contains(t, e["hint"], "mixtape setup")
}

func TestStopDuringSetupEndsTheSessionAsStopped(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	// `mixtape stop` arrives while the session is still setting up.
	h.deps.services.signals = func() (<-chan os.Signal, func()) {
		ch := make(chan os.Signal, 1)
		ch <- syscall.SIGTERM
		return ch, func() {}
	}
	require.Equal(t, ExitOK, h.run("record", plURL))
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.Equal(t, session.PhaseFinished, st.Phase)
	assert.Equal(t, session.ResultStopped, st.Result, "the session says it was stopped, not starting")
}

func TestRecordingKeepsTheMacAwakeUntilItEnds(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.deps.services.openCapture = func(captureRequest) (capture.Source, error) { return &loudSource{}, nil }
	// The setup guard's signals never fire; the session's own handler
	// gets a stop at once, so the session ends as soon as it runs.
	calls := 0
	h.deps.services.signals = func() (<-chan os.Signal, func()) {
		calls++
		ch := make(chan os.Signal, 1)
		if calls > 1 {
			ch <- syscall.SIGTERM
		}
		return ch, func() {}
	}
	require.Equal(t, ExitOK, h.run("record", plURL))
	h.world.mu.Lock()
	defer h.world.mu.Unlock()
	assert.Equal(t, 1, h.world.awakeCalls, "a session that plays keeps the Mac awake")
	assert.Zero(t, h.world.awake, "and lets it sleep again when it ends")
}

func TestRecordSetupFailureIsRecordedInTheSessionState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.world.perms[permission.SystemAudio] = permission.Undecided

	require.Equal(t, ExitPermission, h.run("record", plURL))
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.Equal(t, session.PhaseFinished, st.Phase)
	assert.Equal(t, session.ResultError, st.Result)
	assert.Contains(t, st.Error, "may not record system audio")
}

func TestSetupOpensSettingsAndWaitsForTheTapPermission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.world.perms[permission.SystemAudio] = permission.Undecided
	h.world.perms[permission.ScreenRecording] = permission.Granted // the common mix-up
	h.world.grantAfter[permission.SystemAudio] = 65                // switched on about a minute later
	h.deps.services.openCapture = func(captureRequest) (capture.Source, error) { return &loudSource{}, nil }

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "iTerm needs permission: add iTerm under")
	assert.Contains(t, out, "Click + under it, choose iTerm")
	assert.Contains(t, out, `iTerm is already on in the first list, "Screen & System Audio Recording"`)
	assert.Contains(t, out, `choose "Later"`)
	assert.Contains(t, out, "still waiting (0:30)")
	assert.Contains(t, out, "still waiting (1:00)")
	assert.Contains(t, out, "iTerm may now record system audio")
	assert.Contains(t, out, "saved capture = tap")
	assert.Contains(t, h.world.scripts, "open "+permission.SettingsURL(permission.SystemAudio))
}

func TestSetupPlaysSpotifyForTheTestCapture(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.world.player.snap = spotify.Snapshot{
		State: spotify.StatePaused, Volume: 100,
		Track: spotify.Track{URI: "spotify:track:" + trkA, Name: "Never Gonna Give You Up"},
	}
	h.deps.services.openCapture = func(captureRequest) (capture.Source, error) { return &loudSource{}, nil }

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	assert.Contains(t, h.stdout.String(), `Playing "Never Gonna Give You Up" in Spotify for a 4-second test`)
	cmds := h.world.player.Commands()
	assert.Contains(t, cmds, "play")
	assert.Equal(t, "pause", cmds[len(cmds)-1], "Spotify is paused again after the test")
}

func TestSetupSilentWhilePausedIsNotBlamedOnThePermission(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	// The player stays paused whatever mixtape asks, and the capture is silent.
	h.world.player.snap = spotify.Snapshot{
		State: spotify.StatePaused, Volume: 100,
		Track: spotify.Track{URI: "spotify:track:" + trkA, Name: "Song"},
	}

	require.Equal(t, ExitError, h.run("setup", "--yes"))
	assert.Contains(t, h.stderr.String(), "spotify was not playing during the test")
	assert.NotContains(t, h.stdout.String(), "quit and reopen", "no permission advice for an idle player")
}
