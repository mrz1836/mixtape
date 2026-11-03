package capture

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// RateRange is a span of nominal sample rates an output device offers;
// Min equals Max for a single rate.
type RateRange struct {
	Min, Max float64
}

// OutputDevice describes an output device: the default output, as macOS
// plays through it.
type OutputDevice struct {
	UID   string
	Name  string
	Rate  int // nominal rate, Hz
	Rates []RateRange
}

// Supports reports whether the device offers rate.
func (d OutputDevice) Supports(rate int) bool {
	r := float64(rate)
	return slices.ContainsFunc(d.Rates, func(span RateRange) bool { return r >= span.Min-0.5 && r <= span.Max+0.5 })
}

// RateList names the rates the device offers, for people:
// "44100, 48000, 88200 or 96000 Hz", or a span "8000–192000 Hz".
func (d OutputDevice) RateList() string {
	if len(d.Rates) == 0 {
		return "no rate it reports"
	}
	parts := make([]string, 0, len(d.Rates))
	for _, r := range d.Rates {
		if r.Min == r.Max {
			parts = append(parts, strconv.FormatFloat(r.Min, 'f', -1, 64))
			continue
		}
		parts = append(parts, strconv.FormatFloat(r.Min, 'f', -1, 64)+"–"+strconv.FormatFloat(r.Max, 'f', -1, 64))
	}
	if len(parts) == 1 {
		return parts[0] + " Hz"
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " or " + parts[len(parts)-1] + " Hz"
}

// OutputControl reads and sets the nominal rate of output devices: Core
// Audio in capture/tap, a fake in tests.
type OutputControl interface {
	// DefaultOutput describes the device macOS plays through.
	DefaultOutput() (OutputDevice, error)
	// OutputRate is the nominal rate of the device with uid; an error
	// wrapping ErrOutputGone when there is no such device now.
	OutputRate(uid string) (int, error)
	// SetOutputRate sets the rate of the device with uid and returns once
	// the device runs at it and stays there, or with the reason it does
	// not (ErrOutputReverted when another app set it back).
	SetOutputRate(ctx context.Context, uid string, rate int) error
}

// Errors of output rate control.
var (
	// ErrOutputGone reports an output device that is not connected.
	ErrOutputGone = errors.New("the output device is not connected")
	// ErrOutputReverted reports a rate another app set back right away
	// (call apps such as Microsoft Teams hold the device at their rate).
	ErrOutputReverted = errors.New("another app set the output rate back")
	// ErrOutputTimeout reports a device that did not switch in time.
	ErrOutputTimeout = errors.New("the output device did not switch in time")
)

// OutputRestore records an output rate switch that must be undone: the
// device, the rate it ran at, and the rate a session set. It is saved
// before the switch, so a crash leaves it behind for the next session (or
// `mixtape stop`) to undo. The zero value records nothing.
type OutputRestore struct {
	DeviceUID  string    `json:"device_uid"`
	DeviceName string    `json:"device_name,omitempty"`
	Original   int       `json:"original_rate"`
	Set        int       `json:"set_rate"`
	SessionID  string    `json:"session_id,omitempty"`
	At         time.Time `json:"at"`
}

// Pending reports whether r records a switch to undo.
func (r OutputRestore) Pending() bool { return r.DeviceUID != "" && r.Original > 0 && r.Set > 0 }

// RestoreOutcome is what RestoreOutput did.
type RestoreOutcome int

// Restore outcomes.
const (
	// OutputRestored: the device ran at the rate set; the original is back.
	OutputRestored RestoreOutcome = iota + 1
	// OutputChanged: the device runs at another rate, changed by someone
	// since; it is left alone and the record is done.
	OutputChanged
	// OutputKept: the device is not connected, or would not switch back;
	// the record stays pending (see the error).
	OutputKept
)

// RestoreOutput puts r's original rate back on its device (found by UID,
// wherever the default output is now) when the device still runs at the
// rate r set: a change made since wins.
func RestoreOutput(ctx context.Context, c OutputControl, r OutputRestore) (RestoreOutcome, error) {
	now, err := c.OutputRate(r.DeviceUID)
	if err != nil {
		return OutputKept, err
	}
	if now != r.Set {
		return OutputChanged, nil
	}
	if err := c.SetOutputRate(ctx, r.DeviceUID, r.Original); err != nil {
		return OutputKept, err
	}
	return OutputRestored, nil
}

// OutputSwitch runs one session's output rate switch: Begin sets the rate
// the session asks for, End puts the original back. Nothing about it can
// fail a session: whatever goes wrong is a warning, and the rate stays as
// it was.
type OutputSwitch struct {
	Control OutputControl
	// Save persists the restore record before the switch, and the zero
	// record once it is undone; nil saves nothing.
	Save func(OutputRestore) error
	// Warn receives what went wrong; nil discards it.
	Warn func(msg string)
	// SessionID names the session in the record.
	SessionID string
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	held OutputRestore // the switch in effect, if any
}

// Begin switches the default output device to rate. carry is a switch an
// interrupted session left in place: when the default output is that
// device and still runs at the rate it set, which is rate, the switch is
// taken over as it is (no second switch), and End puts the first
// session's original back. It reports whether the device now runs at rate
// because of a switch End will undo.
func (s *OutputSwitch) Begin(ctx context.Context, rate int, carry *OutputRestore) bool {
	dev, err := s.Control.DefaultOutput()
	if err != nil {
		s.warn("could not read the output device, so its rate is left alone: %v", err)
		return false
	}
	if carry != nil && carry.Pending() && carry.DeviceUID == dev.UID && carry.Set == rate && dev.Rate == rate {
		r := *carry
		r.SessionID = s.SessionID
		if !s.save(r, "") {
			return false
		}
		s.held = r
		return true
	}
	if dev.Rate == rate {
		return false // already there: nothing to switch, nothing to undo
	}
	if !dev.Supports(rate) {
		s.warn("%s does not run at %d Hz (it offers %s): the output stays at %d Hz", dev.Name, rate, dev.RateList(), dev.Rate)
		return false
	}
	r := OutputRestore{
		DeviceUID: dev.UID, DeviceName: dev.Name, Original: dev.Rate, Set: rate, SessionID: s.SessionID, At: s.now(),
	}
	if !s.save(r, fmt.Sprintf(", so %s stays at %d Hz", dev.Name, dev.Rate)) {
		return false // never switch what could not be put back after a crash
	}
	// The switch, and undoing a half-made one, finish even when the session
	// is being stopped: their waits are bounded.
	ctx = context.WithoutCancel(ctx)
	if err := s.Control.SetOutputRate(ctx, dev.UID, rate); err != nil {
		s.warn("could not run %s at %d Hz (%v): the output stays at %d Hz", dev.Name, rate, err, dev.Rate)
		if outcome, rerr := RestoreOutput(ctx, s.Control, r); outcome == OutputKept {
			s.warn("could not put %s back to %d Hz yet (%v): 'mixtape stop' or the next session does", dev.Name, dev.Rate, rerr)
			return false
		}
		s.save(OutputRestore{}, "")
		return false
	}
	s.held = r
	return true
}

// Active returns the switch in effect.
func (s *OutputSwitch) Active() (OutputRestore, bool) { return s.held, s.held.Pending() }

// End puts the original rate back on the switched device, unless it runs
// at another rate by now (that change wins), and clears the record. A
// restore that fails stays recorded for 'mixtape stop' or the next
// session. It is idempotent.
func (s *OutputSwitch) End(ctx context.Context) {
	r := s.held
	if !r.Pending() {
		return
	}
	s.held = OutputRestore{}
	outcome, err := RestoreOutput(context.WithoutCancel(ctx), s.Control, r)
	if outcome == OutputKept {
		s.warn("could not put %s back to %d Hz (%v): 'mixtape stop' or the next session does", r.DeviceName, r.Original, err)
		return
	}
	s.save(OutputRestore{}, "")
}

// save persists r, warning (with consequence appended) when it cannot.
func (s *OutputSwitch) save(r OutputRestore, consequence string) bool {
	if s.Save == nil {
		return true
	}
	if err := s.Save(r); err != nil {
		s.warn("could not save the output rate to restore%s: %v", consequence, err)
		return false
	}
	return true
}

func (s *OutputSwitch) warn(format string, args ...any) {
	if s.Warn != nil {
		s.Warn(fmt.Sprintf(format, args...))
	}
}

func (s *OutputSwitch) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}
