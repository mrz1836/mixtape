package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/session"
)

var errNoCacheDir = errors.New("find the recorder lock: $HOME is not defined")

// runCanceled runs args with a context that is already canceled, as after
// a Ctrl-C.
func (h *harness) runCanceled(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	ctx, cancel := context.WithCancel(h.t.Context())
	cancel()
	return run(ctx, h.deps, args)
}

func TestStatusTextDescribesEachKindOfSession(t *testing.T) {
	t.Parallel()
	exit := func(n int) *int { return &n }
	tests := map[string]struct {
		state   func(h *harness) session.State
		running bool
		want    []string
		not     []string
	}{
		"running, with what is left": {
			running: true,
			state: func(h *harness) session.State {
				return session.State{
					ID: runningID, PID: 4242, Phase: session.PhaseRecording, StartedAt: h.now().Add(-75 * time.Second),
					Library: "/Volumes/Music/Mixtape", Events: "/home/sessions/s1.ndjson", ETASeconds: 400,
					Counts: session.Counts{Total: 3, Recorded: 1, Remaining: 2},
				}
			},
			want: []string{
				"● recording session s1 (pid 4242), running 1:15",
				"songs:   1 recorded, 0 copied, 0 skipped, 0 failed, 2 remaining · ~7m left",
				"library: /Volumes/Music/Mixtape",
				"events:  /home/sessions/s1.ndjson",
			},
		},
		"finished": {
			state: func(h *harness) session.State {
				end := h.now().Add(-5 * time.Minute)
				return session.State{
					ID: "s2", Phase: session.PhaseFinished, Result: session.ResultComplete, ExitCode: exit(0),
					FinishedAt: &end, ETASeconds: 400, Counts: session.Counts{Total: 2, Recorded: 2},
					Current: &session.Current{Title: "Leftover"},
				}
			},
			want: []string{"○ last session s2 finished 5m ago: complete (exit 0)", "2 recorded"},
			not:  []string{"left", "now:", "Leftover"},
		},
		"finished before it could say when": {
			state: func(h *harness) session.State {
				return session.State{
					ID: "s3", Phase: session.PhaseFinished, Result: session.ResultError, ExitCode: exit(ExitNotReady),
					UpdatedAt: h.now().Add(-2 * time.Hour), Error: "spotify is not running",
				}
			},
			want: []string{"last session s3 finished 2h00m ago: error (exit 3)", "error:   spotify is not running"},
		},
		"stopped without finishing": {
			state: func(h *harness) session.State {
				return session.State{ID: "s4", Phase: session.PhaseRecording, StartedAt: h.now().Add(-time.Hour)}
			},
			want: []string{"! last session s4 stopped without finishing (killed or crashed)"},
			not:  []string{"library:", "error:", "events:"},
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.running {
				h.runningSession()
			}
			require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], tc.state(h)))

			require.Equal(t, ExitOK, h.run("status", "-o", "text"), h.stderr.String())
			out := h.stdout.String()
			for _, w := range tc.want {
				assert.Contains(t, out, w)
			}
			for _, n := range tc.not {
				assert.NotContains(t, out, n)
			}
		})
	}
}

func TestStatusSeesASessionThatHasNotWrittenItsStateYet(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	// The state on disk is an older session's; the lock names the new one.
	code := 0
	require.NoError(t, session.WriteState(home, session.State{
		ID: "old", Phase: session.PhaseFinished, Result: session.ResultComplete, ExitCode: &code,
	}))
	started := h.now()
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 777, SessionID: "fresh", Home: home, StartedAt: started})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	require.Equal(t, ExitOK, h.run("status"))
	doc := h.json()
	assert.Equal(t, true, doc["running"])
	st := doc["session"].(map[string]any)
	assert.Equal(t, "fresh", st["session_id"])
	assert.InDelta(t, 777, st["pid"], 0)
	assert.Equal(t, session.PhaseStarting, st["phase"])
}

func TestStatusFollowCopiesTheEventLog(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*harness, string) {
		t.Helper()
		h := newHarness(t)
		lock, st := h.runningSession()
		code := 0
		h.eventsLog(st.Events,
			engine.Event{Type: engine.EventTrackStarted, Track: &engine.EventTrack{Title: "Song", Artist: "Band", DurationMS: 64333}},
			engine.Event{Type: engine.EventSessionFinished, Result: engine.OutcomeComplete, Counts: &session.Counts{Total: 1, Recorded: 1}, ExitCode: &code},
		)
		// A line another version could write, or a torn one: never fatal.
		f, err := os.OpenFile(st.Events, os.O_APPEND|os.O_WRONLY, 0o600)
		require.NoError(t, err)
		_, err = f.WriteString("not an event\n")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		h.world.onSleep = func() { _ = lock.Release() }
		return h, st.Events
	}

	t.Run("verbatim in JSON mode", func(t *testing.T) {
		t.Parallel()
		h, events := setup(t)
		require.Equal(t, ExitOK, h.run("status", "--follow"), h.stderr.String())
		data, err := os.ReadFile(events)
		require.NoError(t, err)
		assert.Equal(t, string(data), h.stdout.String(), "scripts get the event log as it is")
	})

	t.Run("as log lines for people", func(t *testing.T) {
		t.Parallel()
		h, _ := setup(t)
		require.Equal(t, ExitOK, h.run("status", "--follow", "-o", "text"), h.stderr.String())
		out := h.stdout.String()
		assert.Equal(t, "▶ Band — Song (1:04)\ncomplete: 1 recorded, 0 copied, 0 skipped\n", out)
	})
}

func TestStatusFollowNeedsAnEventLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{ID: "s5", Phase: session.PhaseRecording}))

	require.Equal(t, ExitNotFound, h.run("status", "--follow"))
	assert.Equal(t, "start one with 'mixtape record'", h.errDoc()["hint"])
}

func TestSessionCommandsReportAMissingLockLocation(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"status", "wait", "stop"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.deps.services.lockPath = func() (string, error) { return "", errNoCacheDir }
			require.Equal(t, ExitError, h.run(cmd))
			assert.Equal(t, errNoCacheDir.Error(), h.errDoc()["message"])
			assert.Empty(t, h.world.killed)
		})
	}
}

func TestWaitExitsWithAFailedSessionsCodeAndError(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	code := ExitNotReady
	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{
		ID: "s2", Phase: session.PhaseFinished, Result: session.ResultError, ExitCode: &code,
		Error: "spotify is not running",
	}))

	require.Equal(t, ExitNotReady, h.run("wait"))
	assert.Equal(t, "s2", h.json()["session"].(map[string]any)["session_id"], "the session is printed first")
	e := h.errDoc()
	assert.Equal(t, "not_ready", e["code"], "the session's own exit code, as a code")
	assert.Equal(t, "session s2 ended error: spotify is not running", e["message"])
}

func TestWaitOnACrashedSessionSaysHowToResume(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{ID: "s3", Phase: session.PhaseRecording}))

	require.Equal(t, ExitError, h.run("wait"))
	doc := h.json()
	assert.Equal(t, true, doc["stale"])
	e := h.errDoc()
	assert.Equal(t, errCrashed.Error(), e["message"])
	assert.Equal(t, "run the same record command again: it resumes", e["hint"])
}

func TestCtrlCInterruptsWaitAndStopButNotTheSession(t *testing.T) {
	t.Parallel()
	for _, cmd := range []string{"wait", "stop"} {
		t.Run(cmd, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.runningSession()
			require.Equal(t, ExitInterrupted, h.runCanceled(cmd))
			assert.Equal(t, "interrupted", h.errDoc()["code"])
			if cmd == "wait" {
				assert.Empty(t, h.world.killed, "waiting never signals the session")
			}
		})
	}
}

func TestStopNoWaitReturnsOnceTheSignalIsSent(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runningSession()

	require.Equal(t, ExitOK, h.run("stop", "--no-wait"))
	assert.Equal(t, map[string]any{"stopping": true, "session_id": runningID, "pid": float64(4242)}, h.json())
	assert.Equal(t, []int{4242}, h.world.killed)

	require.Equal(t, ExitOK, h.run("stop", "--no-wait", "-o", "text"))
	assert.Equal(t, "stopping session s1 (pid 4242)\n", h.stdout.String())
}

func TestStopGivesUpOnASessionThatKeepsRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runningSession()
	start := h.now()

	require.Equal(t, ExitTimeout, h.run("stop", "-o", "text"))
	assert.Equal(t, "stopping session s1…\n", h.stdout.String())
	assert.Contains(t, h.stderr.String(), "session s1 did not stop within 2m0s")
	assert.Contains(t, h.stderr.String(), "fix: the session is still running: wait again, or 'mixtape stop' it")
	assert.Equal(t, stopTimeout, h.now().Sub(start), "it waited the whole two minutes")
	assert.Equal(t, []int{4242}, h.world.killed, "one signal, not a stream of them")
}

func TestStopReportsASignalItCouldNotSend(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runningSession()
	h.world.killErr = syscall.EPERM

	require.Equal(t, ExitPermission, h.run("stop"))
	assert.Equal(t, "signal session s1 (pid 4242): operation not permitted", h.errDoc()["message"])
}

func TestStopCannotSignalWhatItCannotIdentify(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	h := newHarness(t)
	h.runningSession()
	unreadableLock(t, h.world.lockPath)

	require.Equal(t, ExitPermission, h.run("stop"))
	assert.True(t, strings.HasPrefix(h.errDoc()["message"].(string), "open the recorder lock"))
	assert.Empty(t, h.world.killed, "no signal without knowing who holds the lock")
}
