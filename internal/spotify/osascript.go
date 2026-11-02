package spotify

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

// defaultScriptTimeout bounds one osascript call, so a hung Spotify (or a
// pending permission prompt) can never freeze the caller.
const defaultScriptTimeout = 10 * time.Second

// osascriptPath is the system AppleScript runner.
const osascriptPath = "/usr/bin/osascript"

// Runner executes AppleScript. The real implementation is OSAScript; tests
// substitute a fake so nothing ever talks to the Spotify app.
type Runner interface {
	// Run executes script once, passing args to its `on run argv`
	// handler, and returns its trimmed stdout. On failure the error text
	// includes osascript's stderr.
	Run(ctx context.Context, script string, args ...string) (string, error)
	// Start launches a long-running script and streams the lines it logs
	// (AppleScript `log` writes to stderr), each stamped on arrival. The
	// final Line carries the process exit error, then the channel closes.
	// Canceling ctx kills the process.
	Start(ctx context.Context, script string, args ...string) (<-chan Line, error)
}

// Line is one line streamed from a long-running script.
type Line struct {
	Text string
	At   time.Time // arrival time, on the monotonic clock
	Err  error     // set only on the final line: how the process exited
}

// OSAScript runs AppleScript with /usr/bin/osascript.
type OSAScript struct {
	// Timeout bounds each Run call; zero means 10s. Start is bounded only
	// by its context.
	Timeout time.Duration
}

var _ Runner = (*OSAScript)(nil)

// errNoOutput reports a streamed script that exited without an error but
// also without being asked to (the loop scripts never return).
var errNoOutput = errors.New("osascript exited")

// Run implements Runner.
func (o *OSAScript) Run(ctx context.Context, script string, args ...string) (string, error) {
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = defaultScriptTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, osascriptPath, scriptArgs(script, args)...) //nolint:gosec // G204: fixed binary; scripts are package constants and args are validated values
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("osascript: %w", ctxErr)
		}
		return "", fmt.Errorf("osascript: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// Start implements Runner.
func (o *OSAScript) Start(ctx context.Context, script string, args ...string) (<-chan Line, error) {
	cmd := exec.CommandContext(ctx, osascriptPath, scriptArgs(script, args)...) //nolint:gosec // G204: fixed binary; scripts are package constants and args are validated values
	cmd.Stdout = io.Discard
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("osascript: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("osascript: %w", err)
	}

	out := make(chan Line, 16)
	go func() {
		defer close(out)
		last := streamLines(ctx, stderr, out)
		waitErr := cmd.Wait()
		switch {
		case ctx.Err() != nil:
			waitErr = ctx.Err()
		case waitErr != nil:
			waitErr = fmt.Errorf("osascript: %w: %s", waitErr, last)
		default:
			waitErr = errNoOutput
		}
		select {
		case out <- Line{At: time.Now(), Err: waitErr}:
		case <-ctx.Done():
		}
	}()
	return out, nil
}

// maxLineBytes bounds one streamed line; a longer one (system log noise)
// is cut, and the lines after it still arrive.
const maxLineBytes = 1 << 20

// streamLines sends each line of r to out, stamped on arrival, until r
// ends, and returns the last line. It always reads r to its end, so the
// script never blocks on a full pipe.
func streamLines(ctx context.Context, r io.Reader, out chan<- Line) string {
	br := bufio.NewReader(r)
	var last string
	for {
		text, err := readLine(br, maxLineBytes)
		if text != "" || err == nil {
			last = text
			select {
			case out <- Line{Text: text, At: time.Now()}:
			case <-ctx.Done():
			}
		}
		if err != nil {
			return last
		}
	}
}

// readLine returns the next line of br without its line ending, cut to
// limit bytes. The error is set only when br has nothing more.
func readLine(br *bufio.Reader, limit int) (string, error) {
	var line []byte
	for {
		frag, err := br.ReadSlice('\n')
		if room := limit - len(line); room > 0 {
			line = append(line, frag[:min(len(frag), room)]...)
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue // the rest of a long line
		}
		return strings.TrimRight(string(line), "\r\n"), err
	}
}

// scriptArgs builds the osascript argument list: the script inline, then
// the values its `on run argv` handler receives.
func scriptArgs(script string, args []string) []string {
	out := make([]string, 0, 2+len(args))
	out = append(out, "-e", script)
	return append(out, args...)
}
