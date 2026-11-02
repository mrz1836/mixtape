package cli

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
