package cli

import (
	"context"
	"fmt"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/session"
)

// outputRestorePath is the per-user record of an output rate a session
// switched (next to the recorder lock).
func (a *app) outputRestorePath() (string, error) {
	lockPath, err := a.deps.services.lockPath()
	if err != nil {
		return "", err
	}
	return session.OutputRestorePath(lockPath), nil
}

// pendingOutput reads what an interrupted session left switched, if
// anything; a record that cannot be read is a warning.
func (a *app) pendingOutput() (capture.OutputRestore, string) {
	path, err := a.outputRestorePath()
	if err != nil {
		return capture.OutputRestore{}, ""
	}
	r, err := session.ReadOutputRestore(path)
	if err != nil {
		a.out.Warn("%v", err)
		return capture.OutputRestore{}, path
	}
	return r, path
}

// recoverOutput puts back the output rate an interrupted session switched,
// before anything plays. The recorder lock is held, so that session is
// gone. When this session records with the tap at the very rate it set,
// on the device that is still the default output and still runs at it,
// the switch is not undone just to be made again: it is returned for the
// tap to take over, and this session's end puts the original back.
func (a *app) recoverOutput(ctx context.Context, method capture.Method, want int) *capture.OutputRestore {
	r, path := a.pendingOutput()
	if !r.Pending() {
		return nil
	}
	ctl := a.deps.services.output()
	if method == capture.MethodTap && want == r.Set {
		if dev, err := ctl.DefaultOutput(); err == nil && dev.UID == r.DeviceUID && dev.Rate == r.Set {
			return &r
		}
	}
	if done := a.restoreOutput(ctx, path, r); done != "" {
		a.out.Warn("restored %s (interrupted session %s had switched it to %d Hz)", done, r.SessionID, r.Set)
	}
	return nil
}

// restoreOutput undoes the switch r records, and describes what it put
// back for people ("" when nothing was). A device that cannot be put back
// yet keeps its record, with a warning.
func (a *app) restoreOutput(ctx context.Context, path string, r capture.OutputRestore) string {
	outcome, err := capture.RestoreOutput(ctx, a.deps.services.output(), r)
	switch outcome {
	case capture.OutputRestored, capture.OutputChanged:
		if werr := session.WriteOutputRestore(path, capture.OutputRestore{}); werr != nil {
			a.out.Warn("%v", werr)
		}
	case capture.OutputKept:
		a.out.Warn("%s still runs at %d Hz, set by session %s, and could not be put back to %d Hz yet: %v",
			outputName(r), r.Set, r.SessionID, r.Original, err)
		return ""
	}
	if outcome != capture.OutputRestored {
		return "" // changed since: the new rate stays
	}
	return fmt.Sprintf("%s back to %d Hz", outputName(r), r.Original)
}

// outputName names the device of r for people.
func outputName(r capture.OutputRestore) string {
	if r.DeviceName != "" {
		return r.DeviceName
	}
	return "the output device " + r.DeviceUID
}

// outputRequest fills the output rate part of a capture request for a
// session that records with method: only the process tap switches the
// rate (a capture device is clocked by its Multi-Output Device).
func (a *app) outputRequest(r *captureRequest, run *recordRun, method capture.Method, pending *capture.OutputRestore) {
	want := run.settings.OutputRate
	if want <= 0 {
		return
	}
	if method != capture.MethodTap {
		a.out.Warn("output_rate %d applies to the process tap only: with %s capture the output rate is left alone", want, method)
		return
	}
	path, err := a.outputRestorePath()
	if err != nil {
		a.out.Warn("the output rate is left alone: %v", err)
		return
	}
	r.OutputRate, r.Pending, r.SessionID = want, pending, run.id
	r.SaveRestore = func(rec capture.OutputRestore) error { return session.WriteOutputRestore(path, rec) }
	r.Warn = func(msg string) { a.out.Warn("%s", msg) }
}
