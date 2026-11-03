package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/mrz1836/mixtape/internal/fileutil"
)

// EnvDetached marks the background child of `record --detach`; its value
// is the session ID the parent reserved.
const EnvDetached = "MIXTAPE_DETACHED_SESSION"

// ErrDetachFailed reports a background session that exited before it
// started recording.
var ErrDetachFailed = errors.New("the background session exited before it started")

// Spawn starts exe with args as a detached background process: a new
// session (setsid, so no SIGHUP from the terminal), stdin from /dev/null,
// and stdout and stderr appended to logPath. env is added to the current
// environment. It returns the child's PID without waiting for it.
func Spawn(exe string, args, env []string, logPath string) (*os.Process, error) {
	logFile, err := fileutil.OpenAppend(logPath)
	if err != nil {
		return nil, fmt.Errorf("open session log: %w", err)
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return nil, fmt.Errorf("start background session: %w", err)
	}
	defer func() { _ = devNull.Close() }()

	cmd := exec.Command(exe, args...) //nolint:gosec,noctx // G204: re-executes this binary; the child must outlive any context
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devNull, logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start background session: %w", err)
	}
	return cmd.Process, nil
}

// AwaitStart waits until the child holds the recorder lock at lockPath
// under sessionID, or the child exits, or ctx ends. exited reports
// whether the child is gone (it is reaped by the caller's process).
func AwaitStart(ctx context.Context, lockPath, sessionID string, exited func() bool) error {
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		info, held, err := Inspect(lockPath)
		if err == nil && held && info.SessionID == sessionID {
			return nil
		}
		if exited() {
			return ErrDetachFailed
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// Alive reports whether a process with pid exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
