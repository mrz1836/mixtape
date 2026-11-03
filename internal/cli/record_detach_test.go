package cli

import (
	"errors"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/session"
)

var (
	errNoExecutable = errors.New("executable path unknown")
	errForkFailed   = errors.New("fork/exec: resource temporarily unavailable")
)

// startHelper starts a short-lived stand-in for the background recorder:
// a real child process, so the parent can wait for it and see it exit.
func startHelper(t *testing.T, script string) *os.Process {
	t.Helper()
	proc, err := os.StartProcess("/bin/sh", []string{"sh", "-c", script}, &os.ProcAttr{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = proc.Kill() })
	return proc
}

// spawnCall is what `record --detach` asked to start.
type spawnCall struct {
	exe, logPath string
	args, env    []string
	proc         *os.Process
}

// detachedID returns the session ID the parent reserved for its child.
func (c spawnCall) detachedID() string {
	for _, kv := range c.env {
		if id, ok := strings.CutPrefix(kv, session.EnvDetached+"="); ok {
			return id
		}
	}
	return ""
}

// fakeSpawn starts a helper for each spawn and records the call. With
// takeLock, the "child" takes the recorder lock under the reserved session
// ID, as a real background recorder does once it starts.
func fakeSpawn(t *testing.T, h *harness, script string, takeLock bool) *[]spawnCall {
	t.Helper()
	var (
		mu    sync.Mutex
		calls []spawnCall
	)
	h.deps.services.spawn = func(exe string, args, env []string, logPath string) (*os.Process, error) {
		c := spawnCall{exe: exe, args: args, env: env, logPath: logPath, proc: startHelper(t, script)}
		if takeLock {
			lock, err := session.Acquire(h.world.lockPath, session.LockInfo{
				PID: c.proc.Pid, SessionID: c.detachedID(), Home: h.env["MIXTAPE_HOME"],
			})
			require.NoError(t, err)
			t.Cleanup(func() { _ = lock.Release() })
		}
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, c)
		return c.proc, nil
	}
	return &calls
}

func TestDetachStartsTheRecorderInTheBackground(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	lib := t.TempDir()
	h.deps.services.executable = func() (string, error) { return "/opt/mixtape/bin/mixtape", nil }
	calls := fakeSpawn(t, h, "sleep 10", true)

	require.Equal(t, ExitOK, h.run("record", "--detach", "-L", lib, "--limit", "3", "--into", "Party", plURL), h.stderr.String())
	require.Len(t, *calls, 1)
	c := (*calls)[0]
	id := c.detachedID()
	require.NotEmpty(t, id, "the child is told which session it is")

	doc := h.json()
	assert.Equal(t, id, doc["session_id"])
	assert.InDelta(t, c.proc.Pid, doc["pid"], 0)
	assert.Equal(t, session.EventsPath(home, id), doc["events"])
	assert.Equal(t, session.LogPath(home, id), doc["log"])
	assert.Equal(t, c.logPath, doc["log"], "the child writes the log the parent names")

	assert.Equal(t, "/opt/mixtape/bin/mixtape", c.exe, "the child is this binary")
	require.GreaterOrEqual(t, len(c.args), 7)
	assert.Equal(t, []string{"record", "--home", home, "-o", "text", "--no-color"}, c.args[:6],
		"the child writes plain text to its log, under this home")
	assert.Equal(t, plURL, c.args[len(c.args)-1], "the links come last")
	assert.ElementsMatch(t, []string{"--library=" + lib, "--limit=3", "--into=Party"}, c.args[6:len(c.args)-1],
		"the record flags are forwarded, --detach is not")
	assert.False(t, slices.ContainsFunc(c.args, func(a string) bool { return strings.HasPrefix(a, "--detach") }))
	assert.Empty(t, h.world.killed, "a child that started is left running")
	assert.Empty(t, h.world.player.Commands(), "the parent never touches Spotify")
}

func TestDetachTellsPeopleHowToFollowTheSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	calls := fakeSpawn(t, h, "sleep 10", true)

	require.Equal(t, ExitOK, h.run("record", "--detach", "-o", "text", plURL), h.stderr.String())
	c := (*calls)[0]
	out := h.stdout.String()
	assert.Contains(t, out, "recording in the background: session "+c.detachedID())
	assert.Contains(t, out, "follow: mixtape status --follow")
	assert.Contains(t, out, "stop:   mixtape stop")
	assert.Contains(t, out, "log:    "+session.LogPath(h.env["MIXTAPE_HOME"], c.detachedID()))
}

func TestDetachReportsAChildThatExitedBeforeStarting(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	calls := fakeSpawn(t, h, "exit 3", false)

	require.Equal(t, ExitError, h.run("record", "--detach", plURL))
	c := (*calls)[0]
	e := h.errDoc()
	assert.Contains(t, e["message"], session.ErrDetachFailed.Error())
	assert.Contains(t, e["message"], "see "+c.logPath, "the error names the log that says why")
	assert.Equal(t, []int{c.proc.Pid}, h.world.killed, "the child is never left running unseen")
	assert.Empty(t, h.stdout.String(), "no session to report")
}

func TestDetachRefusesWhileASessionRuns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.runningSession()
	h.deps.services.spawn = func(string, []string, []string, string) (*os.Process, error) {
		t.Error("no second recorder is started")
		return nil, os.ErrInvalid
	}

	require.Equal(t, ExitBusy, h.run("record", "--detach", plURL))
	e := h.errDoc()
	assert.Equal(t, "another mixtape recording session is running (session s1, pid 4242)", e["message"])
	assert.Contains(t, e["hint"], "'mixtape stop' ends it")
}

func TestDetachFailuresBeforeTheChildRuns(t *testing.T) {
	t.Parallel()
	t.Run("the binary cannot be found", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.deps.services.executable = func() (string, error) { return "", errNoExecutable }
		h.deps.services.spawn = func(string, []string, []string, string) (*os.Process, error) {
			t.Error("nothing is spawned")
			return nil, os.ErrInvalid
		}
		require.Equal(t, ExitError, h.run("record", "--detach", plURL))
		assert.Equal(t, errNoExecutable.Error(), h.errDoc()["message"])
	})

	t.Run("the child cannot be started", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.deps.services.spawn = func(string, []string, []string, string) (*os.Process, error) { return nil, errForkFailed }
		require.Equal(t, ExitError, h.run("record", "--detach", plURL))
		assert.Equal(t, errForkFailed.Error(), h.errDoc()["message"])
		assert.Empty(t, h.world.killed, "there is nothing to stop")
	})
}

func TestDetachedChildRecordsUnderTheReservedSession(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	h.world.addPlaylist("Done List", songs()...)
	h.seedLibrary(playlistSpec("Done List"), songs()...)
	h.env[session.EnvDetached] = "20261101-120000-abcdef"
	h.deps.services.spawn = func(string, []string, []string, string) (*os.Process, error) {
		t.Error("the child records itself; it does not detach again")
		return nil, os.ErrInvalid
	}

	require.Equal(t, ExitOK, h.run("record", "--detach", plURL), h.stderr.String())
	st, err := session.ReadState(home)
	require.NoError(t, err)
	assert.Equal(t, "20261101-120000-abcdef", st.ID, "the parent already told the user this ID")
	assert.Equal(t, session.LogPath(home, st.ID), st.Log, "status can point at the child's log")
	assert.Equal(t, session.EventsPath(home, st.ID), st.Events)
	assert.Equal(t, session.PhaseFinished, st.Phase)
}
