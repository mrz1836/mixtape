package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/session"
)

// unreadableLock makes the recorder lock impossible to open from now on,
// and restores it when the test ends.
func unreadableLock(t *testing.T, path string) {
	t.Helper()
	require.NoError(t, os.Chmod(path, 0o000))
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
}

func TestStopDoesNotReportAStopItCouldNotSee(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	h := newHarness(t)
	h.runningSession()
	// The signal is sent, then the lock cannot be read any more.
	h.world.onKill = func(int) { unreadableLock(t, h.world.lockPath) }

	code := h.run("stop")
	assert.NotEqual(t, ExitOK, code, "a session that may still run is not reported stopped")
	assert.Contains(t, h.errDoc()["message"], "check whether session s1 stopped")
}

func TestStatusFollowGivesUpWhenTheLockStaysUnreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	h := newHarness(t)
	_, st := h.runningSession()
	h.eventsLog(st.Events)
	h.world.onSleep = func() { unreadableLock(t, h.world.lockPath) }

	code := h.run("status", "--follow")
	assert.NotEqual(t, ExitOK, code)
	assert.Contains(t, h.errDoc()["message"], "check whether session s1 is still running")
}

func TestDetachRefusesWhileTheLockIsUnreadable(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	require.NoError(t, os.WriteFile(h.world.lockPath, nil, 0o600))
	unreadableLock(t, h.world.lockPath)
	h.deps.services.spawn = func(string, []string, []string, string) (*os.Process, error) {
		t.Error("no background session is started while the lock cannot be checked")
		return nil, os.ErrInvalid
	}

	assert.Equal(t, ExitPermission, h.run("record", "--detach", plURL))
}

func TestStatusFollowWaitsForAJustStartedSessionsLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	// The detached child holds the lock but has written neither its
	// session.json nor its event log yet.
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: runningID, Home: home})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	sleeps := 0
	h.world.onSleep = func() {
		sleeps++
		if sleeps == 2 { // the child gets going: its log appears, and it ends
			h.eventsLog(session.EventsPath(home, runningID), engine.Event{Type: engine.EventWarning, Message: "hello"})
			_ = lock.Release()
		}
	}

	require.Equal(t, ExitOK, h.run("status", "--follow", "-o", "json"), h.stderr.String())
	assert.Contains(t, h.stdout.String(), `"message":"hello"`)
}

func TestStatusFollowOfASessionThatNeverLogs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: runningID, Home: h.env["MIXTAPE_HOME"]})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })

	require.Equal(t, ExitNotFound, h.run("status", "--follow"))
	assert.Contains(t, h.errDoc()["message"], "has not started its event log")
}
