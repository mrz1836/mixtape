package cli

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var (
	errPrefsUnreadable = errors.New("prefs: permission denied")
	errNoDevices       = errors.New("core audio: no devices")
	errNoDefaultOutput = errors.New("core audio: no default output")
)

// TestDoctorChecks drives each readiness check through `doctor -o json`:
// its status, what it says, how to fix it, and whether it blocks (exit 3)
// or only warns (exit 0).
func TestDoctorChecks(t *testing.T) {
	t.Parallel()
	iterm := permission.HostApp{Name: "iTerm"}
	interrupted := func(h *harness, spotifyState *session.SpotifyState) {
		require.NoError(h.t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{
			ID: "old", Phase: session.PhaseRecording, Spotify: spotifyState,
		}))
	}
	writeConfig := func(h *harness, body string) {
		require.NoError(h.t, os.WriteFile(filepath.Join(h.env["MIXTAPE_HOME"], "config.json"), []byte(body), 0o600))
	}
	useDevice := func(h *harness) { h.env["MIXTAPE_CAPTURE"] = "device" }
	multiOutput := func(h *harness) {
		h.world.output = deviceInfo("Multi-Output Device")
	}
	off, on := false, true

	tests := map[string]struct {
		setup  func(h *harness)
		id     string
		status string
		detail string // contained in the detail
		fix    string // contained in the fix; "" means no fix
		exit   int
		more   func(t *testing.T, doc map[string]any)
	}{
		"spotify not installed": {
			setup: func(h *harness) { h.world.installed = false },
			id:    "spotify_installed", status: checkFail, detail: "not installed", fix: "spotify.com/download", exit: ExitNotReady,
		},
		"spotify not running": {
			setup: func(h *harness) { h.world.running = false },
			id:    "automation", status: checkSkip, detail: "checked once Spotify is running", exit: ExitNotReady,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, checkFail, findCheck(t, doc, "spotify_running")["status"])
				assert.Equal(t, checkSkip, findCheck(t, doc, "volume")["status"], "volume is read from Spotify")
			},
		},
		"automation denied in System Settings": {
			setup: func(h *harness) { h.world.perms[permission.Automation] = permission.Denied },
			id:    "automation", status: checkFail, detail: "iTerm may not control Spotify",
			fix: permission.Instructions(permission.Automation, iterm), exit: ExitNotReady,
		},
		"automation refused when Spotify is asked": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: spotify.ErrNotAuthorized} },
			id:    "automation", status: checkFail, detail: "macOS denied control of Spotify", fix: "Automation", exit: ExitNotReady,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, checkSkip, findCheck(t, doc, "volume")["status"], "no answer, no volume")
			},
		},
		"automation prompt on screen": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: spotify.ErrPromptPending} },
			id:    "automation", status: checkWarn, detail: "macOS is asking for permission", fix: "approve the Automation prompt", exit: ExitOK,
		},
		"apple events blocked": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: spotify.ErrAppleEventsBlocked} },
			id:    "automation", status: checkFail, detail: "Apple Events to Spotify are blocked", fix: "normal terminal session", exit: ExitNotReady,
		},
		"spotify answers with another error": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: errOsascriptTimeout} },
			id:    "automation", status: checkWarn, detail: errOsascriptTimeout.Error(), exit: ExitOK,
		},
		"spotify prefs unreadable": {
			setup: func(h *harness) {
				h.deps.services.prefs = func(string) (spotify.Prefs, error) { return spotify.Prefs{}, errPrefsUnreadable }
			},
			id: "crossfade", status: checkWarn, detail: "could not read Spotify's preferences: " + errPrefsUnreadable.Error(), exit: ExitOK,
		},
		"spotify prefs not found": {
			setup: func(h *harness) { h.world.prefs = spotify.Prefs{} },
			id:    "crossfade", status: checkWarn, detail: "preferences were not found; make sure crossfade is off", exit: ExitOK,
		},
		"spotify keeps crossfade elsewhere": {
			setup: func(h *harness) { h.world.prefs.Stale, h.world.prefs.Crossfade = true, true },
			id:    "crossfade", status: checkWarn, detail: "keeps crossfade where mixtape cannot read it",
			fix: `turn off "Crossfade songs"`, exit: ExitOK,
		},
		"automix on": {
			setup: func(h *harness) { h.world.prefs.Automix = true },
			id:    "crossfade", status: checkFail, detail: "automix is on: songs blend into each other", fix: `turn off "Automix"`, exit: ExitNotReady,
		},
		"tap requested on a Mac without it": {
			setup: func(h *harness) {
				h.env["MIXTAPE_CAPTURE"] = "tap"
				h.world.tap = capture.TapStatus{Reason: "macOS 13.6"}
			},
			id: "capture", status: checkFail, detail: "macOS 13.6", fix: "mixtape config set capture device", exit: ExitNotReady,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Empty(t, doc["capture"], "no method would work")
				assert.Equal(t, checkWarn, findCheck(t, doc, "tap")["status"])
				assert.Equal(t, "unavailable: macOS 13.6", findCheck(t, doc, "tap")["detail"])
			},
		},
		"capture devices cannot be listed": {
			setup: func(h *harness) { useDevice(h); h.world.devicesErr = errNoDevices },
			id:    "device", status: checkFail, detail: errNoDevices.Error(), exit: ExitNotReady,
		},
		"default output unreadable": {
			setup: func(h *harness) {
				useDevice(h)
				h.deps.services.defaultOutput = func() (device.Info, error) { return device.Info{}, errNoDefaultOutput }
			},
			id: "routing", status: checkWarn, detail: "could not read the default output: " + errNoDefaultOutput.Error(), exit: ExitOK,
		},
		"routed through a multi-output device": {
			setup: func(h *harness) { useDevice(h); multiOutput(h) },
			id:    "routing", status: checkOK, detail: "system output is Multi-Output Device", exit: ExitOK,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, "device", doc["capture"])
				assert.Equal(t, "BlackHole 2ch", findCheck(t, doc, "device")["detail"])
			},
		},
		"microphone not yet asked": {
			setup: func(h *harness) {
				useDevice(h)
				multiOutput(h)
				h.world.perms[permission.Microphone] = permission.Undecided
			},
			id: string(permission.Microphone), status: checkWarn, detail: "iTerm has not been allowed to open the capture device",
			fix: "macOS asks the first time", exit: ExitOK,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, "Microphone access", findCheck(t, doc, string(permission.Microphone))["name"])
			},
		},
		"system audio granted": {
			id: string(permission.SystemAudio), status: checkOK, detail: "iTerm may record system audio", exit: ExitOK,
		},
		"system audio denied": {
			setup: func(h *harness) { h.world.perms[permission.SystemAudio] = permission.Denied },
			id:    string(permission.SystemAudio), status: checkFail, detail: "iTerm is not allowed to record system audio",
			fix: permission.Instructions(permission.SystemAudio, iterm), exit: ExitNotReady,
		},
		"system audio unknown": {
			setup: func(h *harness) { h.world.perms[permission.SystemAudio] = permission.Unknown },
			id:    string(permission.SystemAudio), status: checkWarn, detail: "macOS did not say whether iTerm may record system audio",
			fix: "short test capture", exit: ExitOK,
		},
		"afconvert missing": {
			setup: func(h *harness) {
				h.deps.services.encoder = nil
				h.deps.services.lookPath = func(string) (string, error) { return "", exec.ErrNotFound }
			},
			id: "afconvert", status: checkFail, detail: "not found", fix: "afconvert ships with macOS", exit: ExitNotReady,
		},
		"config file is not JSON": {
			setup: func(h *harness) { writeConfig(h, "{") },
			id:    "config", status: checkFail, fix: "fix the JSON, or delete the file to start over", exit: ExitNotReady,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, checkOK, findCheck(t, doc, "library")["status"], "the other checks go on")
			},
		},
		"config value is invalid": {
			setup: func(h *harness) { writeConfig(h, `{"capture":"magic"}`) },
			id:    "config", status: checkFail, detail: `"magic"`, fix: "fix the value with 'mixtape config set'", exit: ExitNotReady,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.Equal(t, "tap (setting: auto)", findCheck(t, doc, "capture")["detail"], "the default method is checked instead")
			},
		},
		"config has an unknown key": {
			setup: func(h *harness) { writeConfig(h, `{"later":1}`) },
			id:    "config", status: checkWarn, detail: `"later"`, exit: ExitOK,
		},
		"no session": {
			id: "session", status: checkOK, detail: "none running", exit: ExitOK,
		},
		"a session is recording": {
			setup: func(h *harness) { h.runningSession() },
			id:    "session", status: checkWarn, detail: "session s1 is recording (pid 4242)", fix: "'mixtape stop' ends it", exit: ExitOK,
		},
		"a session was interrupted": {
			setup: func(h *harness) { interrupted(h, nil) },
			id:    "session", status: checkWarn, detail: "session old was interrupted", exit: ExitOK,
			more: func(t *testing.T, doc map[string]any) {
				t.Helper()
				assert.NotContains(t, findCheck(t, doc, "session")["detail"], "Spotify settings")
			},
		},
		"a session was interrupted with Spotify settings changed": {
			setup: func(h *harness) {
				interrupted(h, &session.SpotifyState{
					Original: session.PlayerSettings{Shuffle: &on}, Set: session.PlayerSettings{Shuffle: &off},
				})
			},
			id: "session", status: checkWarn,
			detail: "session old was interrupted; Spotify settings it changed are restored by the next 'mixtape record'",
			exit:   ExitOK,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			require.Equal(t, tc.exit, h.run("doctor"), h.stderr.String())
			doc := h.json()
			assert.Equal(t, tc.exit == ExitOK, doc["ready"])
			c := findCheck(t, doc, tc.id)
			assert.Equal(t, tc.status, c["status"], "%v", c)
			assert.Contains(t, c["detail"], tc.detail)
			if tc.fix == "" {
				assert.NotContains(t, c, "fix")
			} else {
				assert.Contains(t, c["fix"], tc.fix)
			}
			if tc.more != nil {
				tc.more(t, doc)
			}
		})
	}
}

func deviceInfo(name string) device.Info {
	return device.Info{Name: name, Output: true, DefaultOutput: true}
}

func TestDoctorWarnsWhenTheSessionLockCannotBeRead(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root opens any file")
	}
	h := newHarness(t)
	require.NoError(t, os.WriteFile(h.world.lockPath, nil, 0o600))
	unreadableLock(t, h.world.lockPath)

	require.Equal(t, ExitOK, h.run("doctor"), "an unknown session does not block recording checks")
	c := findCheck(t, h.json(), "session")
	assert.Equal(t, checkWarn, c["status"])
	assert.Contains(t, c["detail"], "open the recorder lock")
}

func TestDoctorTextMarksEachOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.running = false
	h.world.prefs = spotify.Prefs{}

	require.Equal(t, ExitNotReady, h.run("doctor", "-o", "text"))
	out := h.stdout.String()
	assert.Contains(t, out, "✓ Spotify app")
	assert.Regexp(t, `✗ Spotify running\s+not running\n    fix: open Spotify \(open -a Spotify\)`, out,
		"a failed check says how to fix it")
	assert.Contains(t, out, "! Crossfade off")
	assert.Regexp(t, `· Automation permission\s+checked once Spotify is running\n`, out, "a skipped check has no fix")
	assert.Contains(t, out, "not ready: 1 check(s) failed")
	e := h.stderr.String()
	assert.Contains(t, e, "mixtape: doctor: not ready: 1 check(s) failed")
	assert.Contains(t, e, "fix: fix the failed checks above, or run 'mixtape setup'")
}
