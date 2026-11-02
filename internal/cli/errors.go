package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	selfupdate "github.com/mrz1836/go-selfupdate"
	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/capture/tap"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// Exit codes form the public CLI contract: scripts and agents branch on
// these values, so their numbers never change.
const (
	// ExitOK means success: the work completed, or a session stopped on request.
	ExitOK = 0
	// ExitError is the catch-all failure.
	ExitError = 1
	// ExitUsage means the command line was wrong: unknown flag or command,
	// bad argument, invalid setting.
	ExitUsage = 2
	// ExitNotReady means a prerequisite is missing: setup not done, Spotify
	// not running, crossfade on, capture not routed.
	ExitNotReady = 3
	// ExitNotFound means a link or session does not exist.
	ExitNotFound = 4
	// ExitPermission means macOS or the filesystem refused: Automation or
	// audio-capture permission, an unwritable library, an update refused.
	ExitPermission = 5
	// ExitBusy means another recorder or library writer holds the lock.
	ExitBusy = 6
	// ExitIncomplete means a session finished with tracks that failed, or
	// idled out before everything was recorded.
	ExitIncomplete = 7
	// ExitTimeout means `wait` gave up while the session is still running.
	ExitTimeout = 124
	// ExitInterrupted means the user forced a quit (a second Ctrl-C).
	ExitInterrupted = 130
)

// Machine-readable error codes, one per exit code.
const (
	codeError       = "error"
	codeUsage       = "usage"
	codeNotReady    = "not_ready"
	codeNotFound    = "not_found"
	codePermission  = "permission"
	codeBusy        = "busy"
	codeIncomplete  = "incomplete"
	codeTimeout     = "timeout"
	codeInterrupted = "interrupted"
)

// codeFor returns the machine code for an exit code.
func codeFor(exit int) string {
	switch exit {
	case ExitUsage:
		return codeUsage
	case ExitNotReady:
		return codeNotReady
	case ExitNotFound:
		return codeNotFound
	case ExitPermission:
		return codePermission
	case ExitBusy:
		return codeBusy
	case ExitIncomplete:
		return codeIncomplete
	case ExitTimeout:
		return codeTimeout
	case ExitInterrupted:
		return codeInterrupted
	}
	return codeError
}

var (
	errFlagConflict = errors.New("--verbose and --quiet are mutually exclusive")
	errBadOutput    = errors.New("invalid --output")
	errInvalidFlag  = errors.New("invalid flags")
	errNotReady     = errors.New("mixtape is not ready to record")
	errNotRunning   = errors.New("no recording session is running")
	errTimeout      = errors.New("timed out waiting for the session")
)

// Error is a failure with a stable contract: a machine code, a one-line
// message, an optional hint telling the user how to fix it, and the exit
// code. Every error a command returns is resolved to one by [mapErr].
type Error struct {
	Code    string
	Message string
	Hint    string
	Exit    int
	Err     error
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Unwrap exposes the cause to errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.Err }

// newError builds an *Error; the code is derived from the exit code.
func newError(exit int, cause error, hint, format string, args ...any) *Error {
	return &Error{
		Code:    codeFor(exit),
		Message: fmt.Sprintf(format, args...),
		Hint:    hint,
		Exit:    exit,
		Err:     cause,
	}
}

// rule maps a domain sentinel to an exit code and a fix hint.
type rule struct {
	target error
	exit   int
	hint   string
}

// rules is the one central sentinel → exit-code table. It is a function,
// not a package variable, so the command tree stays free of globals.
func rules() []rule {
	return []rule{
		// Usage.
		{spotify.ErrBadLink, ExitUsage, "pass a link such as https://open.spotify.com/track/… or a spotify: URI"},
		{spotify.ErrUnsupportedLink, ExitUsage, "mixtape records tracks, albums and playlists"},
		{capture.ErrUnknownMethod, ExitUsage, "use --capture auto, tap or device"},
		{errInvalidFlag, ExitUsage, ""},
		{config.ErrUnknownKey, ExitUsage, "run 'mixtape config show' to list the settings"},
		{config.ErrInvalidValue, ExitUsage, ""},
		{config.ErrDecode, ExitError, "fix the JSON by hand, or delete the file to start over"},
		{errFlagConflict, ExitUsage, ""},
		{errBadOutput, ExitUsage, ""},

		// Not ready.
		{errCrossfade, ExitNotReady, "turn off Spotify → Settings → Playback → Crossfade songs, or pass --allow-crossfade"},
		{engine.ErrCrossfade, ExitNotReady, "turn off Spotify → Settings → Playback → Crossfade songs, then run again"},
		{spotify.ErrNotInstalled, ExitNotReady, "install Spotify from https://www.spotify.com/download"},
		{spotify.ErrNotRunning, ExitNotReady, "open Spotify (open -a Spotify) and run the command again"},
		{spotify.ErrPromptPending, ExitNotReady, "approve the macOS Automation prompt for Spotify, then run again"},
		{errNoAfconvert, ExitNotReady, "afconvert ships with macOS; check /usr/bin/afconvert"},
		{capture.ErrTapUnsupported, ExitNotReady, "use --capture device with BlackHole (see 'mixtape setup')"},
		{device.ErrNoDevice, ExitNotReady, "install BlackHole (brew install blackhole-2ch) and run 'mixtape setup'"},
		{tap.ErrNoProcess, ExitNotReady, "start playing something in Spotify, then run again"},
		{
			engine.ErrSilentDevice, ExitNotReady,
			"route Spotify's output to the capture device with a Multi-Output Device (see 'mixtape setup')",
		},
		{errNotReady, ExitNotReady, "run 'mixtape setup' to fix what failed"},

		// Not found.
		{spotify.ErrNotFound, ExitNotFound, "check the link; private playlists cannot be read"},
		{session.ErrNoSession, ExitNotFound, "start one with 'mixtape record'"},
		{errNotRunning, ExitNotFound, "'mixtape status' shows the last session"},

		// Permission.
		{
			spotify.ErrNotAuthorized, ExitPermission,
			"allow it in System Settings → Privacy & Security → Automation → (your terminal) → Spotify",
		},
		{
			spotify.ErrAppleEventsBlocked, ExitPermission,
			"run mixtape outside the sandbox: Apple Events to Spotify are blocked here",
		},
		{
			engine.ErrSilentTap, ExitPermission,
			"allow your terminal app in System Settings → Privacy & Security → Screen & System Audio Recording, or use --capture device",
		},
		{
			tap.ErrCreateTap, ExitPermission,
			"allow your terminal app in System Settings → Privacy & Security → Screen & System Audio Recording",
		},

		// Busy.
		{session.ErrBusy, ExitBusy, "'mixtape status' shows it; 'mixtape stop' ends it"},
		{library.ErrLocked, ExitBusy, "another mixtape command is writing the library; wait for it to finish"},

		// Errors with a fix.
		{library.ErrUnsafeFix, ExitError, "check that the library drive is mounted; --force rewrites anyway"},
		{spotify.ErrCatalog, ExitError, "check your internet connection and try again"},
		{engine.ErrSpotifyGone, ExitError, "check that Spotify is still open, then run the command again"},
		{errTimeout, ExitTimeout, "the session is still running: wait again, or 'mixtape stop' it"},
		{
			selfupdate.ErrInstallDirNotWritable, ExitPermission,
			"reinstall into a folder you own (the README installs to ~/.local/bin)",
		},
		{
			selfupdate.ErrManagedInstall, ExitPermission,
			"upgrade with the tool that installed mixtape (go install or Homebrew)",
		},
		{os.ErrPermission, ExitPermission, ""},
	}
}

// mapErr resolves any error a command returned into an *Error. nil stays
// nil. An *Error passes through; usage failures, domain sentinels, and
// cancellation are classified; anything else is a general error.
func mapErr(err error) *Error {
	if err == nil {
		return nil
	}
	if e, ok := errors.AsType[*Error](err); ok {
		return e
	}
	if u := asUsageError(err); u != nil {
		return newError(ExitUsage, err, fmt.Sprintf("run '%s --help' for usage", u.cmdPath), "%s", u.err.Error())
	}
	if errors.Is(err, context.Canceled) {
		return newError(ExitInterrupted, err, "", "interrupted")
	}
	for _, r := range rules() {
		if errors.Is(err, r.target) {
			return newError(r.exit, err, r.hint, "%s", err.Error())
		}
	}
	return newError(ExitError, err, "", "%s", err.Error())
}

// usageError marks a cobra argument or flag validation failure (wrong
// positional-arg count, unknown flag, unknown command).
type usageError struct {
	cmdPath string
	err     error
}

func (e *usageError) Error() string { return e.err.Error() }
func (e *usageError) Unwrap() error { return e.err }

// wireUsageErrors wraps every command's positional-arg validator and the
// root's flag-error hook so cobra usage failures surface as *usageError.
// It must run after the command tree is fully built.
func wireUsageErrors(root *cobra.Command) {
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &usageError{cmdPath: cmd.CommandPath(), err: err}
	})
	var wrap func(*cobra.Command)
	wrap = func(c *cobra.Command) {
		if validate := c.Args; validate != nil {
			c.Args = func(cmd *cobra.Command, args []string) error {
				if err := validate(cmd, args); err != nil {
					return &usageError{cmdPath: cmd.CommandPath(), err: err}
				}
				return nil
			}
		}
		for _, sub := range c.Commands() {
			wrap(sub)
		}
	}
	wrap(root)
}

// asUsageError returns err as a *usageError, or nil if it is not a usage
// failure. cobra raises "unknown command" during lookup, before any hook
// runs, so that one is recognized by its stable prefix.
func asUsageError(err error) *usageError {
	if usage, ok := errors.AsType[*usageError](err); ok {
		return usage
	}
	if strings.HasPrefix(err.Error(), "unknown command ") {
		return &usageError{cmdPath: rootName, err: err}
	}
	return nil
}

// errorDoc is the JSON error shape written to stderr.
type errorDoc struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code     string `json:"code"`
	Message  string `json:"message"`
	Hint     string `json:"hint,omitempty"`
	ExitCode int    `json:"exit_code"`
}

// commandLabel returns the command path without the binary name, e.g.
// "config set" for "mixtape config set", or "" for the root.
func commandLabel(path string) string {
	return strings.TrimSpace(strings.TrimPrefix(path, rootName))
}
