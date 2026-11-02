package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/session"
)

// pollEvery is how often wait, stop and status --follow look again;
// stopTimeout bounds how long stop waits for the recorder to exit, and
// lockErrorGrace how long status --follow keeps going while the recorder
// lock cannot be read.
const (
	pollEvery      = 500 * time.Millisecond
	stopTimeout    = 2 * time.Minute
	lockErrorGrace = 30 * time.Second
)

var (
	errCrashed = errors.New("the session ended without finishing (killed or crashed)")
	// errDeadline reports a pollUntil that ran out of time.
	errDeadline = errors.New("deadline passed")
)

// pollUntil calls check every interval until it reports done (or an
// error), ctx ends, or deadline passes (zero: no deadline), which is
// errDeadline. check runs first, before any wait.
func (a *app) pollUntil(ctx context.Context, deadline time.Time, interval time.Duration, check func() (bool, error)) error {
	for {
		done, err := check()
		if done || err != nil {
			return err
		}
		if !deadline.IsZero() && !a.deps.now().Before(deadline) {
			return errDeadline
		}
		if !a.deps.services.sleep(ctx, interval) {
			return ctx.Err()
		}
	}
}

// inspectSession reads the recorder lock: who holds it, if anyone.
func (a *app) inspectSession() (session.LockInfo, bool, error) {
	lockPath, err := a.deps.services.lockPath()
	if err != nil {
		return session.LockInfo{}, false, err
	}
	return session.Inspect(lockPath)
}

// statusDoc is the JSON shape of `status`.
type statusDoc struct {
	Running bool           `json:"running"`
	Stale   bool           `json:"stale,omitempty"`
	Session *session.State `json:"session,omitempty"`
}

// current reads the live or most recent session. running reports whether
// a recorder holds the lock right now.
func (a *app) current() (session.State, bool, error) {
	info, held, err := a.inspectSession()
	if err != nil {
		return session.State{}, false, err
	}
	home := a.home
	if held && info.Home != "" {
		home = info.Home
	}
	st, err := session.ReadState(home)
	switch {
	case held && (errors.Is(err, session.ErrNoSession) || err == nil && st.ID != info.SessionID):
		// The running session has not written its session.json yet (the one
		// there, if any, belongs to an older session in this home).
		return session.State{
			ID: info.SessionID, PID: info.PID, Phase: session.PhaseStarting, StartedAt: info.StartedAt,
			Events: session.EventsPath(home, info.SessionID),
		}, true, nil
	case err != nil:
		return session.State{}, held, err
	}
	return st, held, nil
}

func newStatusCmd(a *app) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show the running (or last) session: progress, current song, ETA",
		Long: `Show the recording session that is running, or the last one if none is.

The JSON form (-o json) is {"running": bool, "session": {...}} where session
is the session.json document: phase, counts, current song, ETA, result and
exit code. "stale": true marks a session that stopped without finishing.

--follow streams the session's events as they happen (NDJSON with -o json)
until it ends.`,
		Example: `  mixtape status
  mixtape status -o json
  mixtape status --follow`,
		GroupID: groupRecording,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, running, err := a.current()
			if err != nil {
				return err
			}
			if follow {
				return a.follow(cmd.Context(), st)
			}
			doc := statusDoc{Running: running, Stale: !running && !st.Finished(), Session: &st}
			return a.out.Result(a.statusText(st, running), doc)
		},
	}
	cmd.Flags().BoolVar(&follow, "follow", false, "Stream the session's events until it ends")
	return cmd
}

// statusText renders a session for people.
func (a *app) statusText(st session.State, running bool) string {
	o := a.out
	var b strings.Builder
	now := a.deps.now()
	switch {
	case running:
		fmt.Fprintf(&b, "%s session %s (pid %d), running %s", o.paint(colorRed, "● "+st.Phase), st.ID, st.PID,
			mmss(now.Sub(st.StartedAt)))
	case st.Finished():
		end := st.UpdatedAt
		if st.FinishedAt != nil {
			end = *st.FinishedAt
		}
		exit := 0
		if st.ExitCode != nil {
			exit = *st.ExitCode
		}
		fmt.Fprintf(&b, "%s last session %s finished %s: %s (exit %d)", o.paint(colorDim, "○"), st.ID,
			ago(now.Sub(end)), st.Result, exit)
	default:
		fmt.Fprintf(&b, "%s last session %s stopped without finishing (killed or crashed)", o.paint(colorYellow, "!"), st.ID)
	}
	if c := st.Current; c != nil && running {
		fmt.Fprintf(&b, "\n  now:     %s  %s / %s", songLabel(c.Artist, c.Title), mmss(seconds(c.Position)), mmss(seconds(c.Duration)))
	}
	cnt := st.Counts
	fmt.Fprintf(&b, "\n  songs:   %d recorded, %d copied, %d skipped, %d failed, %d remaining",
		cnt.Recorded, cnt.Copied, cnt.Skipped, cnt.Failed, cnt.Remaining)
	if running && st.ETASeconds > 0 {
		fmt.Fprintf(&b, " · ~%s left", roundETA(time.Duration(st.ETASeconds)*time.Second))
	}
	if st.Library != "" {
		fmt.Fprintf(&b, "\n  library: %s", st.Library)
	}
	if st.Error != "" {
		fmt.Fprintf(&b, "\n  error:   %s", st.Error)
	}
	if st.Events != "" {
		fmt.Fprintf(&b, "\n  events:  %s", st.Events)
	}
	return b.String()
}

// follow streams a session's event log until the session ends.
func (a *app) follow(ctx context.Context, st session.State) error {
	if st.Events == "" {
		return session.ErrNoSession
	}
	// A session that just started holds the lock a moment before it
	// creates its event log: wait for the log while the session runs.
	err := a.pollUntil(ctx, a.deps.now().Add(lockErrorGrace), pollEvery, func() (bool, error) {
		if _, statErr := os.Stat(st.Events); statErr == nil {
			return true, nil
		}
		_, held, _ := a.inspectSession()
		if !held {
			return false, session.ErrNoSession // it ended before it logged anything
		}
		return false, nil
	})
	if errors.Is(err, errDeadline) {
		err = fmt.Errorf("%w: session %s has not started its event log", session.ErrNoSession, st.ID)
	}
	if err != nil {
		return err
	}
	// A lock that cannot be read says nothing about the session: keep
	// following, and give up only when it stays unreadable.
	var lockErr error
	var failingSince time.Time
	_, err = session.Follow(st.Events, 0, &eventWriter{a: a}, func() bool {
		_, held, inspectErr := a.inspectSession()
		switch {
		case inspectErr == nil && !held:
			return false
		case inspectErr != nil && !held:
			if lockErr == nil {
				failingSince = a.deps.now()
			}
			lockErr = inspectErr
			if a.deps.now().Sub(failingSince) >= lockErrorGrace {
				return false
			}
		default:
			lockErr = nil
		}
		return a.deps.services.sleep(ctx, pollEvery)
	})
	if err == nil && lockErr != nil {
		err = fmt.Errorf("check whether session %s is still running: %w", st.ID, lockErr)
	}
	return err
}

// eventWriter renders the complete NDJSON lines session.Follow copies:
// verbatim in JSON mode, as log lines otherwise.
type eventWriter struct{ a *app }

func (w *eventWriter) Write(p []byte) (int, error) {
	for line := range bytes.Lines(p) {
		if w.a.out.JSON() {
			if _, err := w.a.out.Writer().Write(line); err != nil {
				return 0, err
			}
			continue
		}
		var ev engine.Event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		if ev.Type == engine.EventSessionFinished {
			w.a.out.Println("%s", finishedLine(ev, w.a.out))
		} else if text := eventLine(ev, w.a.out); text != "" {
			w.a.out.Println("%s", text)
		}
	}
	return len(p), nil
}

func finishedLine(ev engine.Event, o *Output) string {
	res := engine.Result{Outcome: ev.Result}
	if ev.Counts != nil {
		res.Counts = *ev.Counts
	}
	return summaryLine(res, o)
}

func newWaitCmd(a *app) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "wait",
		Short: "Block until the session ends; exit with its result",
		Long: `Wait for the running recording session to end, print how it ended, and
exit with the session's own exit code (0 complete or stopped, 7 incomplete,
and so on). If no session is running, the last session's result is
returned at once.

With --timeout, wait gives up after that long and exits 124 while the
session keeps running. Agents should wait in slices shorter than their tool
timeout, for example 'mixtape wait --timeout 9m' in a loop.`,
		Example: `  mixtape wait
  mixtape wait --timeout 9m
  mixtape wait --timeout 30s -o json`,
		GroupID: groupRecording,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.wait(cmd.Context(), timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Give up after this long (exit 124); 0 waits forever")
	return cmd
}

func (a *app) wait(ctx context.Context, timeout time.Duration) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = a.deps.now().Add(timeout)
	}
	var st session.State
	err := a.pollUntil(ctx, deadline, pollEvery, func() (bool, error) {
		var running bool
		var err error
		st, running, err = a.current()
		return !running, err
	})
	switch {
	case errors.Is(err, errDeadline):
		_ = a.out.Result(a.statusText(st, true), statusDoc{Running: true, Session: &st})
		return newError(ExitTimeout, errTimeout, "wait again, or 'mixtape stop' it",
			"session %s is still running after %s", st.ID, timeout)
	case err != nil:
		return err
	}
	return a.reportEnded(st)
}

// reportEnded prints a finished session and returns its exit status.
func (a *app) reportEnded(st session.State) error {
	if err := a.out.Result(a.statusText(st, false), statusDoc{Stale: !st.Finished(), Session: &st}); err != nil {
		return err
	}
	switch {
	case !st.Finished():
		return newError(ExitError, errCrashed, "run the same record command again: it resumes", "%s", errCrashed.Error())
	case st.ExitCode != nil && *st.ExitCode != ExitOK:
		msg := "session " + st.ID + " ended " + st.Result
		if st.Error != "" {
			msg += ": " + st.Error
		}
		return newError(*st.ExitCode, nil, "", "%s", msg)
	}
	return nil
}

func newStopCmd(a *app) *cobra.Command {
	var noWait bool
	cmd := &cobra.Command{
		Use:   "stop",
		Short: "Stop the running session gracefully",
		Long: `Ask the running recording session to stop: the song in progress is
discarded (or kept with --keep-partial on that session), Spotify is paused
and its settings are restored, and the session's summary is printed.

The recorder is found through its lock, so stop never signals a process
that is not a mixtape recorder. --no-wait returns as soon as the request
is sent.`,
		Example: `  mixtape stop
  mixtape stop --no-wait -o json`,
		GroupID: groupRecording,
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.stop(cmd.Context(), noWait)
		},
	}
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "Return once the stop request is sent")
	return cmd
}

type stopDoc struct {
	Stopping  bool   `json:"stopping"`
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
}

func (a *app) stop(ctx context.Context, noWait bool) error {
	info, held, err := a.inspectSession()
	if err != nil {
		return err
	}
	if !held || info.PID <= 0 {
		return errNotRunning
	}
	if killErr := a.deps.services.kill(info.PID, syscall.SIGTERM); killErr != nil {
		if !errors.Is(killErr, syscall.ESRCH) {
			return fmt.Errorf("signal session %s (pid %d): %w", info.SessionID, info.PID, killErr)
		}
		// The session ended between the lock check and the signal: it is
		// stopped either way, so report how it finished.
		noWait = false
	}
	if noWait {
		return a.out.Result(fmt.Sprintf("stopping session %s (pid %d)", info.SessionID, info.PID),
			stopDoc{Stopping: true, SessionID: info.SessionID, PID: info.PID})
	}
	a.out.Println("stopping session %s…", info.SessionID)
	var lockErr error // the last failed look at the lock: no answer, so keep looking
	err = a.pollUntil(ctx, a.deps.now().Add(stopTimeout), pollEvery, func() (bool, error) {
		_, stillHeld, inspectErr := a.inspectSession()
		lockErr = nil
		if !stillHeld {
			lockErr = inspectErr
		}
		return inspectErr == nil && !stillHeld, nil
	})
	switch {
	case errors.Is(err, errDeadline) && lockErr != nil:
		return fmt.Errorf("check whether session %s stopped: %w", info.SessionID, lockErr)
	case errors.Is(err, errDeadline):
		return fmt.Errorf("%w: session %s did not stop within %s", errTimeout, info.SessionID, stopTimeout)
	case err != nil:
		return err
	}
	st, err := session.ReadState(info.Home)
	if err != nil {
		return err
	}
	return a.out.Result(a.statusText(st, false), statusDoc{Session: &st})
}

var _ io.Writer = (*eventWriter)(nil)
