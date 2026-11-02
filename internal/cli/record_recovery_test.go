package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// interruptedWithShuffleAndRepeat leaves session.json as a session killed
// after turning shuffle off and repeat on.
func (h *harness) interruptedWithShuffleAndRepeat() {
	h.t.Helper()
	on, off := true, false
	require.NoError(h.t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{
		ID: "old", Phase: session.PhaseRecording, Spotify: &session.SpotifyState{
			Original: session.PlayerSettings{Shuffle: &on, Repeat: &off},
			Set:      session.PlayerSettings{Shuffle: &off, Repeat: &on},
		},
	}))
}

// copyOnlyRun sets up a playlist that is already recorded, so `record`
// plays nothing.
func (h *harness) copyOnlyRun() {
	h.t.Helper()
	h.world.addPlaylist("Done List", songs()...)
	h.seedLibrary(playlistSpec("Done List"), songs()...)
}

func TestRecordPutsBackSpotifySettingsAnInterruptedSessionChanged(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	h.interruptedWithShuffleAndRepeat()
	// Spotify still holds what the old session set.
	h.world.player.snap = spotify.Snapshot{State: spotify.StatePaused, Volume: 100, Shuffle: false, Repeat: true}

	require.Equal(t, ExitOK, h.run("record", plURL), h.stderr.String())
	assert.Equal(t, []string{"shuffle", "repeat"}, h.world.player.Commands())
	assert.Equal(t, []string{"restored Spotify shuffle, repeat changed by interrupted session old"}, h.warnings())
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.False(t, st.Spotify.NeedsRestore(), "the new session has nothing left to put back")
}

func TestRecoveredSettingsAreMarkedRestored(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.interruptedWithShuffleAndRepeat()
	h.world.player.snap = spotify.Snapshot{Shuffle: false, Repeat: true}
	a := &app{deps: h.deps, out: plainOutput(t, false), home: h.env["MIXTAPE_HOME"]}

	assert.Nil(t, a.recoverSpotify(t.Context(), h.world.player), "nothing is pending")
	st, err := session.ReadState(a.home)
	require.NoError(t, err)
	assert.Equal(t, "old", st.ID)
	require.NotNil(t, st.Spotify)
	assert.True(t, st.Spotify.Restored, "a crash right after this does not put them back twice")
}

func TestRecordLeavesSpotifySettingsTheUserChangedSince(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	h.interruptedWithShuffleAndRepeat()
	// The user turned shuffle back on and repeat off by hand.
	h.world.player.snap = spotify.Snapshot{State: spotify.StatePaused, Volume: 100, Shuffle: true, Repeat: false}

	require.Equal(t, ExitOK, h.run("record", plURL), h.stderr.String())
	assert.Empty(t, h.world.player.Commands(), "the user's own choice wins")
	assert.Empty(t, h.warnings())
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.False(t, st.Spotify.NeedsRestore())
}

func TestRecordKeepsSettingsSpotifyWouldNotLetItRestore(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	h.interruptedWithShuffleAndRepeat()
	h.world.player.snap = spotify.Snapshot{Err: spotify.ErrNotAuthorized}

	require.Equal(t, ExitOK, h.run("record", plURL), h.stderr.String())
	w := h.warnings()
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "could not restore Spotify settings from session old yet: ")
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	require.True(t, st.Spotify.NeedsRestore(), "the next session tries again")
	assert.True(t, *st.Spotify.Original.Shuffle)
}

func TestRecordStopsBeforePlayingWhenSomethingIsMissing(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup   func(h *harness)
		args    []string
		exit    int
		message string
		hint    string
	}{
		"no afconvert": {
			setup: func(h *harness) {
				h.deps.services.encoder = nil
				h.deps.services.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			},
			exit: ExitNotReady, message: errNoAfconvert.Error(), hint: "afconvert ships with macOS",
		},
		"Spotify refuses to be controlled": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: spotify.ErrNotAuthorized} },
			exit:  ExitPermission, hint: "Privacy & Security → Automation",
		},
		"--mute without the tap": {
			args: []string{"--mute", "--capture", "device"}, exit: ExitUsage,
			message: "invalid flags: --mute needs --capture tap", hint: "run 'mixtape record --help' for usage",
		},
		"no capture device": {
			setup: func(h *harness) {
				h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
					return nil, fmt.Errorf("%w: %q", device.ErrNoDevice, r.Device)
				}
			},
			args: []string{"--capture", "device"}, exit: ExitNotReady, hint: "brew install blackhole-2ch",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.world.addPlaylist("List", songs()...)
			if tc.setup != nil {
				tc.setup(h)
			}
			args := append([]string{"record", plURL}, tc.args...)
			require.Equal(t, tc.exit, h.run(args...), h.stderr.String())
			e := h.errDoc()
			if tc.message != "" {
				assert.Equal(t, tc.message, e["message"])
			}
			assert.Contains(t, e["hint"], tc.hint)
			assert.NotContains(t, h.world.player.Commands(), "playtrack", "nothing played")
		})
	}
}

func TestRecordRecordsAFailedSetupInTheSessionState(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)

	require.Equal(t, ExitUsage, h.run("record", plURL, "--mute", "--capture", "device"))
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.Equal(t, session.ResultError, st.Result)
	assert.Equal(t, "invalid flags: --mute needs --capture tap", st.Error, "status shows why it ended")
}

func TestRecordUsesTheCaptureDeviceChosenForThisRun(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	var got []captureRequest
	h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
		got = append(got, r)
		return nil, device.ErrNoDevice
	}

	require.Equal(t, ExitNotReady, h.run("record", plURL, "--capture", "device", "--device", "Loopback"))
	require.Len(t, got, 1)
	assert.Equal(t, captureRequest{Method: capture.MethodDevice, Device: "Loopback"}, got[0],
		"the flags beat the config for this run")
}

func TestRecordWarnsAboutALowSpotifyVolume(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.world.player.snap.Volume = 48
	h.loudCapture()
	// The session's own signal handler gets a stop at once, so the session
	// ends as soon as it runs.
	calls := 0
	h.deps.services.signals = func() (<-chan os.Signal, func()) {
		calls++
		ch := make(chan os.Signal, 1)
		if calls > 1 {
			ch <- syscall.SIGTERM
		}
		return ch, func() {}
	}

	require.Equal(t, ExitOK, h.run("record", plURL), h.stderr.String())
	assert.Contains(t, h.warnings(), "Spotify's volume is 48%: recordings are made at that level (100% is best)")
}

func TestRecordWaitsForNoOtherLibraryWriter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	lib, err := library.Open(h.env["MIXTAPE_LIBRARY"], library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	unlock, err := lib.Lock()
	require.NoError(t, err)
	t.Cleanup(unlock)

	require.Equal(t, ExitBusy, h.run("record", plURL))
	assert.Contains(t, h.errDoc()["hint"], "another mixtape command is writing the library")
	_, held, err := session.Inspect(h.world.lockPath)
	require.NoError(t, err)
	assert.False(t, held, "the recorder lock is not left behind")
}

func TestRecordNeedsAWritableSessionLog(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	// The sessions folder is a file, so no event log can be opened.
	require.NoError(t, os.WriteFile(filepath.Join(h.env["MIXTAPE_HOME"], "sessions"), nil, 0o600))

	require.Equal(t, ExitError, h.run("record", plURL))
	assert.NotEmpty(t, h.errDoc()["message"])
	assert.Empty(t, h.stdout.String(), "no events without a log")
}

func TestRecordDryRunTextExplainsEachAction(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	ref := h.world.addPlaylist("Road Trip", songs()...)
	col := h.world.catalog.cols[ref]
	col.Tracks[0].Playable = false
	h.world.catalog.cols[ref] = col
	other := library.Spec{Kind: library.KindPlaylist, SpotifyID: "0therPlaylist000000000", Name: "Other"}
	h.seedLibrary(other, songs()[1])

	require.Equal(t, ExitOK, h.run("record", plURL, "--dry-run"))
	doc := h.json()
	items := doc["items"].([]any)
	require.Len(t, items, 2)
	assert.Equal(t, "unavailable", items[0].(map[string]any)["hint"])
	assert.Equal(t, "copy", items[1].(map[string]any)["action"])

	require.Equal(t, ExitOK, h.run("record", plURL, "--dry-run", "-o", "text"))
	out := h.stdout.String()
	assert.Contains(t, out, "2 songs: 1 to record (3:33), 1 to copy, 0 already done\nRoad Trip\n")
	assert.Regexp(t, `record\s+1  Rick Astley — Never Gonna Give You Up \(3:33\)  \[unavailable\]`, out,
		"songs the catalog says cannot play are still tried")
	assert.Regexp(t, `copy\s+2  Someone — Second \(3:00\)  ← Other/`, out, "a copy names its source")
}

func TestRecordDryRunOfLiveMode(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("record", "--dry-run"))
	doc := h.json()
	assert.Equal(t, "live", doc["mode"])
	assert.Equal(t, []any{}, doc["items"], "an empty list, not null")

	require.Equal(t, ExitOK, h.run("record", "--dry-run", "--into", "Party", "-o", "text"))
	assert.Equal(t, "Live mode: mixtape records whatever Spotify plays into Party\n", h.stdout.String())
}

func TestRecordFollowsPlaylistsTheCatalogCannotList(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		edit    func(c *spotify.Collection)
		warning string
	}{
		"longer than the embed page": {
			edit:    func(c *spotify.Collection) { c.Truncated, c.Complete = true, false },
			warning: `"Long Mix" lists more than 2 songs; mixtape will follow the rest in Spotify`,
		},
		"no track list at all": {
			edit:    func(c *spotify.Collection) { c.Source, c.Tracks, c.Complete = spotify.SourceOEmbed, nil, false },
			warning: `"Long Mix": the track list is unavailable; mixtape will play it in Spotify and follow it`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			ref := h.world.addPlaylist("Long Mix", songs()...)
			col := h.world.catalog.cols[ref]
			tc.edit(&col)
			h.world.catalog.cols[ref] = col

			require.Equal(t, ExitOK, h.run("record", plURL, "--dry-run", "-o", "text"))
			assert.Contains(t, h.stderr.String(), "mixtape: warning: "+tc.warning)
			assert.Contains(t, h.stdout.String(), "…and the rest of the playlist, followed in Spotify")
		})
	}
}
