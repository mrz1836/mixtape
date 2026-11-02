package cli

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var errProcessList = errors.New("list processes: operation not permitted")

// findCheck returns the doctor check with id.
func findCheck(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	for _, c := range doc["checks"].([]any) {
		if m := c.(map[string]any); m["id"] == id {
			return m
		}
	}
	require.Failf(t, "no such check", "%q in %v", id, doc["checks"])
	return nil
}

func TestDoctorSaysWhenItCannotCheckSpotify(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.running, h.world.runningErr = false, errProcessList

	h.run("doctor")
	doc := h.json()
	c := findCheck(t, doc, "spotify_running")
	assert.Equal(t, checkWarn, c["status"])
	assert.Equal(t, "could not check Spotify: "+errProcessList.Error(), c["detail"])
	assert.Nil(t, c["fix"], "no advice to open an app that may well be open")
	assert.Equal(t, checkOK, findCheck(t, doc, "automation")["status"],
		"Spotify is still asked directly, which never launches it")
}

func TestRecordGoesOnWhenSpotifyCannotBeChecked(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.running, h.world.runningErr = false, errProcessList
	require.NoError(t, (&app{deps: h.deps}).checkRecordReady(&recordFlags{}),
		"the poll that follows reports what is wrong, if anything")

	h.world.runningErr = nil
	require.ErrorIs(t, (&app{deps: h.deps}).checkRecordReady(&recordFlags{}), spotify.ErrNotRunning)
}

func TestSetupSaysWhenItCannotCheckSpotify(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.world.running, h.world.runningErr = false, errProcessList
	h.deps.services.openCapture = func(captureRequest) (capture.Source, error) { return &loudSource{}, nil }

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	assert.Contains(t, h.stdout.String(), "could not check Spotify: "+errProcessList.Error())
	assert.NotContains(t, h.stdout.String(), "Open Spotify, then press Enter")
}

func TestSetupReportsADeviceListingFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
	h.world.tap = capture.TapStatus{Reason: "macOS 13"}
	h.world.devicesErr = errors.New("core audio: no devices") //nolint:err113 // a one-off test failure

	require.Equal(t, ExitError, h.run("setup", "--yes"))
	assert.Contains(t, h.stderr.String(), "list audio devices: core audio: no devices")
	assert.NotContains(t, h.stdout.String(), "brew install", "no advice to install what may be installed")
	assert.Empty(t, h.world.scripts, "nothing is installed")
}
