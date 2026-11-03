package capture

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeOutput is a machine's output devices: rates by UID, the default
// output, and the rate sets asked for.
type fakeOutput struct {
	mu       sync.Mutex
	def      string
	names    map[string]string
	rates    map[string]int
	offers   map[string][]RateRange
	sets     []string
	setErr   error // SetOutputRate refuses with it: the rate stays
	failOnce error // the next SetOutputRate switches, but too late: it fails with this, once
	revertTo int   // another app sets the rate back to this right after a set
	readErr  error
}

func speakers() *fakeOutput {
	return &fakeOutput{
		def:    "spk",
		names:  map[string]string{"spk": "MacBook Pro Speakers", "dac": "USB DAC"},
		rates:  map[string]int{"spk": 48000, "dac": 96000},
		offers: map[string][]RateRange{"spk": {{44100, 44100}, {48000, 48000}, {96000, 96000}}, "dac": {{44100, 192000}}},
	}
}

func (f *fakeOutput) DefaultOutput() (OutputDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return OutputDevice{}, f.readErr
	}
	return OutputDevice{UID: f.def, Name: f.names[f.def], Rate: f.rates[f.def], Rates: f.offers[f.def]}, nil
}

func (f *fakeOutput) OutputRate(uid string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rates[uid]
	if !ok {
		return 0, fmt.Errorf("%w: %s", ErrOutputGone, uid)
	}
	return r, nil
}

func (f *fakeOutput) SetOutputRate(_ context.Context, uid string, rate int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, fmt.Sprintf("%s=%d", uid, rate))
	if _, ok := f.rates[uid]; !ok {
		return ErrOutputGone
	}
	if f.setErr != nil {
		return f.setErr
	}
	f.rates[uid] = rate
	if f.revertTo != 0 {
		f.rates[uid] = f.revertTo
		return ErrOutputReverted
	}
	if err := f.failOnce; err != nil {
		f.failOnce = nil
		return err
	}
	return nil
}

func (f *fakeOutput) rate(uid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rates[uid]
}

func (f *fakeOutput) setsSoFar() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sets...)
}

// store is a fake restore record file.
type store struct {
	saved []OutputRestore
	err   error
}

func (s *store) save(r OutputRestore) error {
	if s.err != nil {
		return s.err
	}
	s.saved = append(s.saved, r)
	return nil
}

func (s *store) last() OutputRestore {
	if len(s.saved) == 0 {
		return OutputRestore{}
	}
	return s.saved[len(s.saved)-1]
}

var errDisk = errors.New("disk full")

func newSwitch(c OutputControl, st *store, warns *[]string) *OutputSwitch {
	return &OutputSwitch{
		Control: c, Save: st.save, SessionID: "s1",
		Warn: func(m string) { *warns = append(*warns, m) },
		Now:  func() time.Time { return time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC) },
	}
}

func TestOutputSwitchSwitchesAndRestores(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	sw := newSwitch(dev, st, &warns)
	require.True(t, sw.Begin(t.Context(), 44100, nil))
	assert.Equal(t, 44100, dev.rate("spk"))
	want := OutputRestore{
		DeviceUID: "spk", DeviceName: "MacBook Pro Speakers", Original: 48000, Set: 44100, SessionID: "s1",
		At: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	}
	require.Len(t, st.saved, 1, "saved before the switch")
	assert.Equal(t, want, st.saved[0])
	held, ok := sw.Active()
	assert.True(t, ok)
	assert.Equal(t, want, held)

	sw.End(t.Context())
	assert.Equal(t, 48000, dev.rate("spk"), "the original rate is back")
	assert.False(t, st.last().Pending(), "the record is cleared")
	assert.Empty(t, warns)
	sw.End(t.Context())
	assert.Equal(t, []string{"spk=44100", "spk=48000"}, dev.setsSoFar(), "End is idempotent")
}

func TestOutputSwitchLeavesTheRateAlone(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		rate  int
		setup func(*fakeOutput, *store)
		warn  string
	}{
		"already at the rate": {rate: 48000},
		"a rate it does not offer": {
			rate: 32000, warn: "MacBook Pro Speakers does not run at 32000 Hz (it offers 44100, 48000 or 96000 Hz): the output stays at 48000 Hz",
		},
		"the record cannot be saved": {
			rate: 44100, setup: func(_ *fakeOutput, s *store) { s.err = errDisk },
			warn: "could not save the output rate to restore, so MacBook Pro Speakers stays at 48000 Hz: disk full",
		},
		"the device cannot be read": {
			rate: 44100, setup: func(f *fakeOutput, _ *store) { f.readErr = ErrOutputGone },
			warn: "could not read the output device, so its rate is left alone: " + ErrOutputGone.Error(),
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dev, st, warns := speakers(), &store{}, []string{}
			if tc.setup != nil {
				tc.setup(dev, st)
			}
			sw := newSwitch(dev, st, &warns)
			assert.False(t, sw.Begin(t.Context(), tc.rate, nil))
			assert.Empty(t, dev.setsSoFar(), "nothing is switched")
			assert.Empty(t, st.saved, "nothing to restore")
			if tc.warn == "" {
				assert.Empty(t, warns)
			} else {
				assert.Equal(t, []string{tc.warn}, warns)
			}
			sw.End(t.Context())
			assert.Empty(t, dev.setsSoFar(), "and nothing to put back")
		})
	}
}

// A call app holding the device at its rate sets it back at once: the
// switch is given up, and the record cleared, since nothing is left to
// undo.
func TestOutputSwitchGivesUpWhenAnotherAppSetsTheRateBack(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	dev.revertTo = 48000
	sw := newSwitch(dev, st, &warns)
	assert.False(t, sw.Begin(t.Context(), 44100, nil))
	assert.Equal(t, 48000, dev.rate("spk"))
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "could not run MacBook Pro Speakers at 44100 Hz")
	assert.Contains(t, warns[0], ErrOutputReverted.Error())
	assert.Equal(t, []string{"spk=44100"}, dev.setsSoFar(), "the other app's rate is not fought")
	require.Len(t, st.saved, 2)
	assert.False(t, st.last().Pending(), "nothing left to restore")
}

// A switch that took too long is undone at once: the device may have
// switched late, and an unverified rate is not one to record at.
func TestOutputSwitchUndoesAHalfMadeSwitch(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	dev.failOnce = ErrOutputTimeout // the rate changed, but too late to be trusted
	sw := newSwitch(dev, st, &warns)
	assert.False(t, sw.Begin(t.Context(), 44100, nil))
	assert.Equal(t, 48000, dev.rate("spk"), "put back right away")
	assert.Equal(t, []string{"spk=44100", "spk=48000"}, dev.setsSoFar())
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], ErrOutputTimeout.Error())
	assert.False(t, st.last().Pending())

	// When putting it back fails too, the record stays for later.
	dev, st, warns = speakers(), &store{}, []string{}
	dev.failOnce = ErrOutputTimeout
	sw = newSwitch(dev, st, &warns)
	sw.Control = refuseAfterFirst{dev}
	assert.False(t, sw.Begin(t.Context(), 44100, nil))
	require.Len(t, warns, 2, "the switch, and the restore that could not finish yet")
	assert.Contains(t, warns[1], "'mixtape stop' or the next session does")
	assert.True(t, st.last().Pending(), "kept for 'mixtape stop' or the next session")
	dev.setErr = nil
	outcome, err := RestoreOutput(t.Context(), dev, st.last())
	require.NoError(t, err)
	assert.Equal(t, OutputRestored, outcome)
	assert.Equal(t, 48000, dev.rate("spk"))
}

// The user's own change wins: End leaves a device that no longer runs at
// the rate the session set alone, even if it is the default output no
// more.
func TestOutputSwitchEndRespectsAChangeMadeSince(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	sw := newSwitch(dev, st, &warns)
	require.True(t, sw.Begin(t.Context(), 44100, nil))
	dev.rates["spk"] = 96000 // Audio MIDI Setup, meanwhile
	dev.def = "dac"
	sw.End(t.Context())
	assert.Equal(t, 96000, dev.rate("spk"))
	assert.Equal(t, []string{"spk=44100"}, dev.setsSoFar())
	assert.False(t, st.last().Pending())
	assert.Empty(t, warns)

	// Restore goes by UID: a device that is no longer the default output is
	// still put back.
	dev, st, warns = speakers(), &store{}, []string{}
	sw = newSwitch(dev, st, &warns)
	require.True(t, sw.Begin(t.Context(), 44100, nil))
	dev.def = "dac"
	sw.End(t.Context())
	assert.Equal(t, 48000, dev.rate("spk"))
	assert.Equal(t, 96000, dev.rate("dac"), "the new default is not touched")
}

func TestOutputSwitchKeepsTheRecordWhenTheDeviceIsGone(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	sw := newSwitch(dev, st, &warns)
	require.True(t, sw.Begin(t.Context(), 44100, nil))
	delete(dev.rates, "spk") // unplugged
	sw.End(t.Context())
	require.Len(t, warns, 1)
	assert.Contains(t, warns[0], "could not put MacBook Pro Speakers back to 48000 Hz")
	assert.True(t, st.last().Pending(), "kept until the device is back")

	dev.rates["spk"] = 44100 // plugged back in, still at the session's rate
	outcome, err := RestoreOutput(t.Context(), dev, st.last())
	require.NoError(t, err)
	assert.Equal(t, OutputRestored, outcome)
	assert.Equal(t, 48000, dev.rate("spk"))
}

// An interrupted session's switch is taken over, not made twice, when this
// session wants the same rate on the same device; its original is what
// comes back at the end.
func TestOutputSwitchCarriesAnInterruptedSwitchForward(t *testing.T) {
	t.Parallel()
	dev, st, warns := speakers(), &store{}, []string{}
	dev.rates["spk"] = 44100
	pending := &OutputRestore{DeviceUID: "spk", DeviceName: "MacBook Pro Speakers", Original: 48000, Set: 44100, SessionID: "old"}
	sw := newSwitch(dev, st, &warns)
	require.True(t, sw.Begin(t.Context(), 44100, pending))
	assert.Empty(t, dev.setsSoFar(), "no second switch")
	require.Len(t, st.saved, 1)
	assert.Equal(t, "s1", st.saved[0].SessionID, "the record now belongs to this session")
	assert.Equal(t, 48000, st.saved[0].Original, "with the first session's original")
	sw.End(t.Context())
	assert.Equal(t, 48000, dev.rate("spk"))

	// Another rate, or another device: not carried.
	for name, p := range map[string]*OutputRestore{
		"another rate":   {DeviceUID: "spk", Original: 48000, Set: 96000},
		"another device": {DeviceUID: "dac", Original: 96000, Set: 44100},
		"nothing":        {},
	} {
		dev, st, warns := speakers(), &store{}, []string{}
		sw := newSwitch(dev, st, &warns)
		require.True(t, sw.Begin(t.Context(), 44100, p), name)
		assert.Equal(t, []string{"spk=44100"}, dev.setsSoFar(), name)
		assert.Equal(t, 48000, st.last().Original, "%s: the original is this device's own rate", name)
	}
}

// refuseAfterFirst lets the first set through, then refuses every other.
type refuseAfterFirst struct{ *fakeOutput }

func (r refuseAfterFirst) SetOutputRate(ctx context.Context, uid string, rate int) error {
	if len(r.setsSoFar()) == 0 {
		return r.fakeOutput.SetOutputRate(ctx, uid, rate)
	}
	return ErrOutputTimeout
}

func TestRestoreOutput(t *testing.T) {
	t.Parallel()
	dev := speakers()
	r := OutputRestore{DeviceUID: "spk", Original: 44100, Set: 48000}
	outcome, err := RestoreOutput(t.Context(), dev, r)
	require.NoError(t, err)
	assert.Equal(t, OutputRestored, outcome)
	assert.Equal(t, 44100, dev.rate("spk"))

	outcome, err = RestoreOutput(t.Context(), dev, r)
	require.NoError(t, err)
	assert.Equal(t, OutputChanged, outcome, "no longer at the rate set: left alone")

	outcome, err = RestoreOutput(t.Context(), dev, OutputRestore{DeviceUID: "gone", Original: 1, Set: 2})
	require.ErrorIs(t, err, ErrOutputGone)
	assert.Equal(t, OutputKept, outcome)

	dev.rates["spk"], dev.setErr = 48000, ErrOutputTimeout
	outcome, err = RestoreOutput(t.Context(), dev, r)
	assert.Equal(t, 48000, dev.rate("spk"), "refused: still at the rate set")
	require.ErrorIs(t, err, ErrOutputTimeout)
	assert.Equal(t, OutputKept, outcome)
}

func TestOutputDeviceRates(t *testing.T) {
	t.Parallel()
	d := OutputDevice{Rates: []RateRange{{44100, 44100}, {48000, 48000}}}
	assert.True(t, d.Supports(44100))
	assert.False(t, d.Supports(96000))
	assert.Equal(t, "44100 or 48000 Hz", d.RateList())
	span := OutputDevice{Rates: []RateRange{{8000, 192000}}}
	assert.True(t, span.Supports(88200))
	assert.False(t, span.Supports(384000))
	assert.Equal(t, "8000–192000 Hz", span.RateList())
	assert.Equal(t, "no rate it reports", OutputDevice{}.RateList())
	assert.False(t, OutputDevice{}.Supports(48000))
	assert.True(t, OutputRestore{DeviceUID: "x", Original: 1, Set: 2}.Pending())
	assert.False(t, OutputRestore{DeviceUID: "x", Set: 2}.Pending())
}
