package cli

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// leftSwitched is the record a session that switched the speakers to
// 44.1 kHz and crashed leaves behind.
func leftSwitched() capture.OutputRestore {
	return capture.OutputRestore{
		DeviceUID: "spk", DeviceName: "MacBook Pro Speakers", Original: 48000, Set: 44100, SessionID: "old",
		At: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC),
	}
}

// outputRecord is where the harness keeps the output restore record.
func (h *harness) outputRecord() string { return session.OutputRestorePath(h.world.lockPath) }

// crashedWithOutput leaves r behind, with the device at the rate r set.
func (h *harness) crashedWithOutput(r capture.OutputRestore) {
	h.t.Helper()
	require.NoError(h.t, session.WriteOutputRestore(h.outputRecord(), r))
	h.world.out.rates[r.DeviceUID] = r.Set
}

// captureRequests records every capture request, and fails each open, so
// a session stops right after asking.
func (h *harness) captureRequests() *[]captureRequest {
	var got []captureRequest
	h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
		got = append(got, r)
		return nil, device.ErrNoDevice
	}
	return &got
}

func TestRecordAsksTheTapForTheOutputRate(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	got := h.captureRequests()
	require.Equal(t, ExitNotReady, h.run("record", plURL, "--output-rate", "44100"))
	require.Len(t, *got, 1)
	r := (*got)[0]
	assert.Equal(t, capture.MethodTap, r.Method)
	assert.Equal(t, 44100, r.OutputRate)
	assert.NotEmpty(t, r.SessionID)
	assert.Nil(t, r.Pending, "nothing to take over")
	require.NotNil(t, r.Warn)
	require.NotNil(t, r.SaveRestore)

	rec := leftSwitched()
	require.NoError(t, r.SaveRestore(rec))
	saved, err := session.ReadOutputRestore(h.outputRecord())
	require.NoError(t, err)
	assert.Equal(t, rec, saved, "the tap's record lands next to the recorder lock")

	*got = nil
	require.Equal(t, ExitNotReady, h.run("record", plURL))
	require.Len(t, *got, 1)
	assert.Zero(t, (*got)[0].OutputRate, "keep: the rate is left alone")
	assert.Nil(t, (*got)[0].SaveRestore)
}

func TestOutputRateIsForTheTapOnly(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.env[config.EnvOutputRate] = "44100"
	got := h.captureRequests()
	require.Equal(t, ExitNotReady, h.run("record", plURL, "--capture", "device"))
	require.Len(t, *got, 1)
	assert.Zero(t, (*got)[0].OutputRate)
	assert.Contains(t, h.warnings(), "output_rate 44100 applies to the process tap only: with device capture the output rate is left alone")
	_, sets := h.world.out.state()
	assert.Empty(t, sets)
}

// A record an interrupted session left is undone before anything plays,
// even by a session that plays nothing.
func TestRecordPutsBackTheOutputRateAnInterruptedSessionLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	h.crashedWithOutput(leftSwitched())
	require.Equal(t, ExitOK, h.run("record", plURL))
	rates, sets := h.world.out.state()
	assert.Equal(t, []string{"spk=48000"}, sets)
	assert.Equal(t, 48000, rates["spk"])
	assert.NoFileExists(t, h.outputRecord(), "done with")
	assert.Contains(t, h.warnings(), "restored MacBook Pro Speakers back to 48000 Hz (interrupted session old had switched it to 44100 Hz)")
}

// A session that would switch to the same rate takes the switch over
// instead of undoing it and making it again.
func TestRecordTakesOverAnInterruptedSwitchItWouldMake(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	h.crashedWithOutput(leftSwitched())
	got := h.captureRequests()
	require.Equal(t, ExitNotReady, h.run("record", plURL, "--output-rate", "44100"))
	require.Len(t, *got, 1)
	require.NotNil(t, (*got)[0].Pending)
	assert.Equal(t, leftSwitched(), *(*got)[0].Pending)
	_, sets := h.world.out.state()
	assert.Empty(t, sets, "no switch back and forth")
	assert.FileExists(t, h.outputRecord(), "the tap takes the record over")

	// Another rate, or another device as the default: undone first.
	for name, setup := range map[string]func(h *harness){
		"another rate": func(h *harness) { h.env[config.EnvOutputRate] = "48000" },
		"another device": func(h *harness) {
			h.env[config.EnvOutputRate] = "44100"
			h.world.out.names["dac"], h.world.out.rates["dac"], h.world.out.def = "USB DAC", 44100, "dac"
		},
		"the device capture": func(h *harness) {
			h.env[config.EnvOutputRate] = "44100"
			h.env[config.EnvCapture] = "device"
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.world.addPlaylist("List", songs()...)
			h.crashedWithOutput(leftSwitched())
			setup(h)
			got := h.captureRequests()
			require.Equal(t, ExitNotReady, h.run("record", plURL))
			require.Len(t, *got, 1)
			assert.Nil(t, (*got)[0].Pending)
			rates, sets := h.world.out.state()
			assert.Equal(t, []string{"spk=48000"}, sets)
			assert.Equal(t, 48000, rates["spk"])
		})
	}
}

func TestRecordLeavesAnOutputRateChangedSince(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	h.crashedWithOutput(leftSwitched())
	h.world.out.rates["spk"] = 48000 // put back by hand meanwhile
	require.Equal(t, ExitOK, h.run("record", plURL))
	_, sets := h.world.out.state()
	assert.Empty(t, sets)
	assert.NoFileExists(t, h.outputRecord())
}

func TestRecordKeepsTheRecordOfADeviceNotConnected(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.copyOnlyRun()
	r := leftSwitched()
	r.DeviceUID, r.DeviceName = "usb", "USB DAC"
	require.NoError(t, session.WriteOutputRestore(h.outputRecord(), r))
	require.Equal(t, ExitOK, h.run("record", plURL))
	assert.FileExists(t, h.outputRecord(), "kept until the DAC is back")
	assert.Contains(t, h.warnings(),
		"USB DAC still runs at 44100 Hz, set by session old, and could not be put back to 48000 Hz yet: "+capture.ErrOutputGone.Error())
}

func TestStopWithNothingRunningPutsBackWhatWasLeft(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.crashedWithOutput(leftSwitched())
	h.interruptedWithShuffleAndRepeat()
	h.world.player.snap = spotify.Snapshot{Shuffle: false, Repeat: true, Volume: 100}

	require.Equal(t, ExitOK, h.run("stop"), h.stderr.String())
	assert.Empty(t, h.warnings(), "the result names what was put back: no warning repeats it")
	doc := h.json()
	assert.Equal(t, false, doc["stopping"])
	assert.Equal(t, []any{"output MacBook Pro Speakers back to 48000 Hz", "Spotify shuffle", "Spotify repeat"}, doc["restored"])
	assert.NoFileExists(t, h.outputRecord())
	assert.Equal(t, 48000, h.world.out.rates["spk"])
	st, err := session.ReadState(h.env["MIXTAPE_HOME"])
	require.NoError(t, err)
	assert.True(t, st.Spotify.Restored)

	require.Equal(t, ExitNotFound, h.run("stop"), "nothing left: as before")
	assert.Equal(t, errNotRunning.Error(), h.errDoc()["message"])

	h.crashedWithOutput(leftSwitched())
	require.Equal(t, ExitOK, h.run("stop", "-o", "text"))
	assert.Equal(t, "no session was running; restored output MacBook Pro Speakers back to 48000 Hz\n", h.stdout.String())
}

func TestStopWithNothingToPutBack(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.crashedWithOutput(leftSwitched())
	h.world.out.rates["spk"] = 48000 // changed since: nothing to put back
	require.Equal(t, ExitNotFound, h.run("stop"))
	assert.NoFileExists(t, h.outputRecord(), "the stale record is gone")
}

func TestDoctorOutputCheck(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		env    map[string]string
		setup  func(*fakeOutput)
		status string
		detail string
	}{
		"keep": {
			status: checkOK,
			detail: "MacBook Pro Speakers at 48 kHz (Spotify's 44.1 kHz streams are resampled to it; output_rate 44100 avoids that)",
		},
		"keep at 44.1 kHz": {setup: func(f *fakeOutput) { f.rates["spk"] = 44100 }, status: checkOK, detail: "MacBook Pro Speakers at 44.1 kHz"},
		"a rate to switch to": {
			env: map[string]string{config.EnvOutputRate: "44100"}, status: checkOK,
			detail: "MacBook Pro Speakers at 48 kHz; switched to 44.1 kHz while recording (output_rate)",
		},
		"already there": {
			env: map[string]string{config.EnvOutputRate: "48000"}, status: checkOK,
			detail: "MacBook Pro Speakers at 48 kHz; already at output_rate",
		},
		"a rate it does not offer": {
			env: map[string]string{config.EnvOutputRate: "96000"}, status: checkWarn,
			detail: "MacBook Pro Speakers at 48 kHz; it does not offer output_rate 96000 (it offers 44100 or 48000 Hz), so it is left alone",
		},
		"the device capture": {
			env: map[string]string{config.EnvOutputRate: "44100", config.EnvCapture: "device"}, status: checkWarn,
			detail: "MacBook Pro Speakers at 48 kHz; output_rate 44100 applies to the process tap only, so it is left alone",
		},
		"no device to read": {
			setup: func(f *fakeOutput) { f.readErr = capture.ErrOutputGone }, status: checkWarn,
			detail: "could not read the default output: " + capture.ErrOutputGone.Error(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			maps.Copy(h.env, tc.env)
			if tc.setup != nil {
				tc.setup(h.world.out)
			}
			h.run("doctor")
			c := findCheck(t, h.json(), checkIDOutput)
			assert.Equal(t, tc.status, c["status"])
			assert.Equal(t, tc.detail, c["detail"])
		})
	}
}

// What a session left changed shows up whatever way it ended.
func TestDoctorReportsAnOutputRateLeftSwitched(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.crashedWithOutput(leftSwitched())
	require.NoError(t, session.WriteState(h.env["MIXTAPE_HOME"], session.State{ID: "old", Phase: session.PhaseFinished}))
	require.Equal(t, ExitOK, h.run("doctor"))
	c := findCheck(t, h.json(), checkIDSession)
	assert.Equal(t, checkWarn, c["status"])
	assert.Equal(t, "none running; left changed by a session: MacBook Pro Speakers at 44100 Hz (was 48000 Hz)", c["detail"])
	assert.Contains(t, c["fix"], "'mixtape stop' puts them back now")
}
