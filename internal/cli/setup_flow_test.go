package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var (
	errBrewFailed = errors.New("exit status 1")
	errTapBusy    = errors.New("tap: another process holds it")
)

// savedConfig reads the config file setup wrote.
func (h *harness) savedConfig() config.File {
	h.t.Helper()
	f, _, err := config.Load(filepath.Join(h.env["MIXTAPE_HOME"], "config.json"))
	require.NoError(h.t, err)
	return f
}

// blackHoleWorld is a Mac without the process tap whose output already
// goes through a Multi-Output Device, so BlackHole hears Spotify.
func (h *harness) blackHoleWorld() {
	h.interactive()
	h.world.tap = capture.TapStatus{Reason: "macOS 13.6"}
	h.world.output = deviceInfo("Multi-Output Device")
	h.loudCapture()
}

func TestSetupStopsWhereNothingCanBeFixedFromHere(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup   func(h *harness)
		exit    int
		message string
		out     string // printed before it stops
	}{
		"Spotify not installed": {
			setup: func(h *harness) { h.world.installed = false },
			exit:  ExitNotReady, message: spotify.ErrNotInstalled.Error(),
		},
		"Automation stays denied": {
			setup: func(h *harness) { h.world.perms[permission.Automation] = permission.Denied },
			exit:  ExitPermission, message: "control of Spotify (Automation)",
			out: "✗ still not granted",
		},
		"Spotify refuses to answer": {
			setup: func(h *harness) { h.world.player.snap = spotify.Snapshot{Err: spotify.ErrNotAuthorized} },
			exit:  ExitPermission, message: spotify.ErrNotAuthorized.Error(),
		},
		"the config file is broken": {
			setup: func(h *harness) {
				require.NoError(h.t, os.WriteFile(filepath.Join(h.env["MIXTAPE_HOME"], "config.json"), []byte("{"), 0o600))
			},
			exit: ExitError,
		},
		"the microphone stays denied": {
			setup: func(h *harness) {
				h.blackHoleWorld()
				h.world.perms[permission.Microphone] = permission.Denied
			},
			exit: ExitPermission, message: "microphone access for the capture device",
			out: "iTerm needs permission",
		},
		"BlackHole hears nothing": {
			setup: func(h *harness) {
				h.blackHoleWorld()
				h.deps.services.openCapture = func(context.Context, captureRequest) (capture.Source, error) {
					return &nopSource{}, nil
				}
				h.world.player.snap = playingSnap(trkA, "Song", 10)
			},
			exit: ExitError, message: "the BlackHole test failed: " + errProbeSilent.Error(),
		},
		"Homebrew cannot install BlackHole": {
			setup: func(h *harness) {
				h.blackHoleWorld()
				h.world.devices = h.world.devices[1:]
				h.deps.services.runScript = func(context.Context, string, ...string) error { return errBrewFailed }
			},
			exit: ExitError, message: "brew install blackhole-2ch: " + errBrewFailed.Error(),
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.interactive()
			h.loudCapture()
			tc.setup(h)
			require.Equal(t, tc.exit, h.run("setup", "--yes", "-o", "text", "--no-color"), h.stdout.String())
			assert.Contains(t, h.stderr.String(), tc.message)
			assert.Contains(t, h.stdout.String(), tc.out)
			assert.NotContains(t, h.stdout.String(), "saved capture", "nothing unverified is saved")
			assert.NoFileExists(t, filepath.Join(h.env["MIXTAPE_HOME"], "config.json.tmp"))
		})
	}
}

func TestSetupShowsTheAutomationPromptAndGoesOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.interactive()
	h.loudCapture()
	// Undecided until macOS asks; the user clicks OK on the prompt.
	h.world.perms[permission.Automation] = permission.Undecided
	h.world.grantAfter[permission.Automation] = 1

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "macOS may ask you to allow it")
	assert.NotContains(t, out, "needs permission", "no detour through System Settings")
	assert.NotContains(t, h.world.scripts, "open "+permission.SettingsURL(permission.Automation))
	assert.Contains(t, out, "saved capture = tap")
}

func TestSetupAsksToOpenSpotifyAndEndsNotReadyWhileItIsClosed(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.interactive()
	h.loudCapture()
	h.world.running = false

	require.Equal(t, ExitNotReady, h.run("setup", "--yes", "-o", "text"))
	out := h.stdout.String()
	assert.Contains(t, out, "Open Spotify, then press Enter.")
	assert.Contains(t, out, "saved capture = tap", "the verified method is still saved")
	assert.Contains(t, out, "not ready: 1 check(s) failed")
	assert.Contains(t, h.stderr.String(), "fix: fix the failed checks above, then run 'mixtape doctor'")
}

func TestSetupPointsAtCrossfadeAndEndsNotReadyIfItStaysOn(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.interactive()
	h.loudCapture()
	h.world.prefs.Crossfade, h.world.prefs.CrossfadeSeconds = true, 6.658

	require.Equal(t, ExitNotReady, h.run("setup", "--yes", "--no-color"))
	out := h.stdout.String()
	assert.Contains(t, out, "✗ crossfade is set to 6.7s: songs blend into each other and cannot be cut\n  "+fixCrossfade)
	assert.Contains(t, out, " off, then press Enter.\n", "setup waits for it to be turned off")
	assert.Equal(t, "tap", *h.savedConfig().Capture)
}

func TestSetupAnswersFromTheKeyboard(t *testing.T) {
	t.Parallel()
	t.Run("Enter takes every default", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.interactive()
		h.loudCapture()
		h.deps.stdin = strings.NewReader("\n\n")

		require.Equal(t, ExitOK, h.run("setup"), h.stderr.String())
		assert.Contains(t, h.stdout.String(), "Use the process tap (records Spotify only, nothing to install)? [Y/n] ")
		f := h.savedConfig()
		assert.Equal(t, "tap", *f.Capture)
		assert.Equal(t, h.env["MIXTAPE_LIBRARY"], *f.Library, "the library offered was the current one")
	})

	t.Run("no to the tap sets up BlackHole", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.interactive()
		h.loudCapture()
		h.world.output = deviceInfo("Multi-Output Device")
		h.deps.stdin = strings.NewReader("n\n\n")
		var methods []capture.Method
		h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
			methods = append(methods, r.Method)
			return &loudSource{}, nil
		}

		require.Equal(t, ExitOK, h.run("setup"), h.stderr.String())
		assert.Equal(t, []capture.Method{capture.MethodDevice}, methods, "the tap is never tried")
		assert.Equal(t, "device", *h.savedConfig().Capture)
	})

	t.Run("no to installing BlackHole", func(t *testing.T) {
		t.Parallel()
		h := newHarness(t)
		h.blackHoleWorld()
		h.world.devices = h.world.devices[1:]
		h.deps.stdin = strings.NewReader("no\n")

		require.Equal(t, ExitNotReady, h.run("setup"))
		assert.Contains(t, h.stderr.String(), "no capture device")
		assert.Empty(t, h.world.scripts, "nothing is installed without a yes")
	})
}

func TestSetupInstallsBlackHoleWithHomebrew(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.blackHoleWorld()
	blackHole := h.world.devices[0]
	h.world.devices = h.world.devices[1:]
	h.deps.services.runScript = func(_ context.Context, name string, args ...string) error {
		h.world.mu.Lock()
		defer h.world.mu.Unlock()
		h.world.scripts = append(h.world.scripts, strings.Join(append([]string{name}, args...), " "))
		if name == "brew" {
			h.world.devices = append(h.world.devices, blackHole)
		}
		return nil
	}

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "The process tap is unavailable here (macOS 13.6); using BlackHole.")
	assert.Contains(t, out, "Install it with Homebrew now (brew install blackhole-2ch)? [Y/n] y")
	assert.Equal(t, []string{"brew install blackhole-2ch"}, h.world.scripts)
	assert.Contains(t, out, "saved capture = device")
	assert.Contains(t, out, "ready to record (device capture)")
}

func TestSetupWalksThroughTheMultiOutputDevice(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.blackHoleWorld()
	h.world.output = deviceInfo("MacBook Pro Speakers")
	// The user creates the Multi-Output Device once Audio MIDI Setup opens.
	h.deps.services.runScript = func(_ context.Context, name string, args ...string) error {
		h.world.mu.Lock()
		defer h.world.mu.Unlock()
		h.world.scripts = append(h.world.scripts, strings.Join(append([]string{name}, args...), " "))
		h.world.output = deviceInfo("Multi-Output Device")
		return nil
	}

	require.Equal(t, ExitOK, h.run("setup", "--yes"), h.stderr.String())
	out := h.stdout.String()
	assert.Contains(t, out, "Route Spotify's audio through BlackHole:")
	assert.Contains(t, out, "Create Multi-Output Device")
	assert.Equal(t, []string{"open -a Audio MIDI Setup"}, h.world.scripts)
	assert.Contains(t, out, "Press Enter when the Multi-Output Device is the sound output.")
	assert.Contains(t, out, "ready to record (device capture)")
}

func TestSetupFallsBackToBlackHoleWhenTheTapFails(t *testing.T) {
	t.Parallel()
	tests := map[string]func() (capture.Source, error){
		"the tap cannot open":  func() (capture.Source, error) { return nil, errTapBusy },
		"the tap cannot start": func() (capture.Source, error) { return &traceSource{startErr: errTapBusy}, nil },
	}
	for name, openTap := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.interactive()
			h.world.output = deviceInfo("Multi-Output Device")
			h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
				if r.Method == capture.MethodTap {
					return openTap()
				}
				return &loudSource{}, nil
			}

			require.Equal(t, ExitOK, h.run("setup", "--yes", "--no-color"), h.stderr.String())
			out := h.stdout.String()
			assert.Contains(t, out, "✗ the tap test failed: "+errTapBusy.Error())
			assert.NotContains(t, out, "quit and reopen", "a failure that is not silence is not the permission")
			assert.Contains(t, out, "Setting up BlackHole instead.")
			assert.Equal(t, "device", *h.savedConfig().Capture)
		})
	}
}

func TestDevicesText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("devices", "-o", "text"))
	out := h.stdout.String()
	assert.True(t, strings.HasPrefix(out, "process tap: available\n"), out)
	assert.Regexp(t, `\n  BlackHole 2ch\s+input, output\n`, out)
	assert.Regexp(t, `\n  MacBook Pro Speakers\s+output, default output\n`, out)

	h.world.tap = capture.TapStatus{Reason: "macOS 13.6"}
	require.Equal(t, ExitOK, h.run("devices", "-o", "text"))
	assert.True(t, strings.HasPrefix(h.stdout.String(), "process tap: unavailable: macOS 13.6\n"))
}

func TestDevicesReportsAListingFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.devicesErr = errNoDevices
	require.Equal(t, ExitError, h.run("devices"))
	assert.Equal(t, errNoDevices.Error(), h.errDoc()["message"])
	assert.Empty(t, h.stdout.String())
}

func TestDevicesListsDefaultInputs(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.devices = []device.Info{{Name: "MacBook Pro Microphone", Input: true, DefaultInput: true}}
	require.Equal(t, ExitOK, h.run("devices"))
	doc := h.json()
	assert.NotContains(t, doc, "default_output", "no default output, no field")
	dev := doc["devices"].([]any)[0].(map[string]any)
	assert.Equal(t, true, dev["default_input"])
	assert.NotContains(t, dev, "default_output")
}
