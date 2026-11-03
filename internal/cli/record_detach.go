package cli

import (
	"context"
	"fmt"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/mrz1836/mixtape/internal/session"
)

// detachDoc is the JSON shape of `record --detach`.
type detachDoc struct {
	SessionID string `json:"session_id"`
	PID       int    `json:"pid"`
	Events    string `json:"events"`
	Log       string `json:"log"`
}

// detach re-runs this record command in the background, with flags (the
// record flags given) and args (the links), and reports how to track it.
func (a *app) detach(ctx context.Context, flags, args []string) error {
	svc := a.deps.services
	exe, err := svc.executable()
	if err != nil {
		return err
	}
	switch info, held, inspectErr := a.inspectSession(); {
	case held:
		return fmt.Errorf("%w (session %s, pid %d)", session.ErrBusy, info.SessionID, info.PID)
	case inspectErr != nil:
		return inspectErr
	}
	lockPath, err := svc.lockPath()
	if err != nil {
		return err
	}
	id := session.NewID(a.deps.now())
	logPath := session.LogPath(a.home, id)
	childArgs := append([]string{"record", "--home", a.home, "-o", formatText, "--no-color"}, flags...)
	childArgs = append(childArgs, args...)
	proc, err := svc.spawn(exe, childArgs, []string{session.EnvDetached + "=" + id}, logPath)
	if err != nil {
		return err
	}
	exited := make(chan struct{})
	go func() {
		_, _ = proc.Wait()
		close(exited)
	}()
	// The child looks every link up before it takes the lock: allow for
	// that, and never leave it running unseen when the wait gives up.
	wctx, cancel := context.WithTimeout(ctx, detachWait+time.Duration(len(args))*spotifyLookupWait)
	defer cancel()
	err = session.AwaitStart(wctx, lockPath, id, func() bool {
		select {
		case <-exited:
			return true
		default:
			return false
		}
	})
	if err != nil {
		_ = svc.kill(proc.Pid, syscall.SIGTERM)
		return fmt.Errorf("%w; see %s", err, logPath)
	}
	doc := detachDoc{SessionID: id, PID: proc.Pid, Events: session.EventsPath(a.home, id), Log: logPath}
	text := fmt.Sprintf("recording in the background: session %s (pid %d)\n  follow: mixtape status --follow\n  wait:   mixtape wait\n  stop:   mixtape stop\n  log:    %s",
		id, proc.Pid, logPath)
	return a.out.Result(text, doc)
}

// childFlags forwards the flags given to this record command, minus
// --detach and the global flags the parent sets itself.
func childFlags(cmd *cobra.Command) []string {
	var out []string
	cmd.Flags().Visit(func(fl *pflag.Flag) {
		switch fl.Name {
		case "detach", flagOutput, flagNoColor, flagHome, flagQuiet:
			return
		}
		out = append(out, "--"+fl.Name+"="+fl.Value.String())
	})
	return out
}
