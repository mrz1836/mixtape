package spotify

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// AppleScript / Apple Event error numbers that matter for classification.
const (
	codeNotAuthorized   = -1743 // errAEEventNotPermitted: Automation denied
	codeEventTimedOut   = -1712 // errAETimeout: usually the permission prompt
	codeProcNotFound    = -600  // procNotFound: app not running (or unreachable)
	codeConnInvalid     = -609  // connectionInvalid: app quit mid-call
	codeLaunchServices  = -10810
	codeLaunchServices2 = -10822 // kLSServerCommunicationErr (sandboxed caller)
)

// ScriptError is a failed AppleScript call: osascript exiting with an
// error, or an error the snapshot script caught and reported itself.
// Classify reads Code, so nothing downstream has to parse the message.
type ScriptError struct {
	// Code is the AppleScript or Apple Event error number (-1743 when
	// Automation is denied), or 0 when the failure gave none.
	Code int
	// Msg is the whole error text, ending with the number when there is
	// one, as osascript prints it.
	Msg string
}

// Error returns Msg.
func (e *ScriptError) Error() string { return e.Msg }

// Classify maps an osascript failure to one of the package's sentinels,
// keeping the original text as detail. running says whether the Spotify
// process exists; it separates "not running" from "Apple Events blocked"
// (a sandbox) for the errors that look alike. Errors that already carry a
// sentinel pass through unchanged. Only a *ScriptError carries an error
// number: any other error is ErrScript, whatever its text says.
func Classify(err error, running bool) error {
	if err == nil {
		return nil
	}
	for _, s := range []error{
		ErrNotInstalled, ErrNotRunning, ErrNotAuthorized,
		ErrAppleEventsBlocked, ErrPromptPending, ErrScript,
	} {
		if errors.Is(err, s) {
			return err
		}
	}
	detail := strings.TrimSpace(err.Error())
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w: %s", ErrPromptPending, detail)
	}
	se, ok := errors.AsType[*ScriptError](err)
	if !ok {
		return fmt.Errorf("%w: %s", ErrScript, detail)
	}
	return classifyCode(se.Code, detail, running)
}

// classifyCode maps an Apple Event error number to a sentinel.
func classifyCode(code int, detail string, running bool) error {
	switch {
	case code == codeNotAuthorized:
		return fmt.Errorf("%w: %s", ErrNotAuthorized, detail)
	case code == codeEventTimedOut:
		return fmt.Errorf("%w: %s", ErrPromptPending, detail)
	case ambiguousCode(code) && running:
		return fmt.Errorf("%w: %s", ErrAppleEventsBlocked, detail)
	case ambiguousCode(code):
		return fmt.Errorf("%w: %s", ErrNotRunning, detail)
	}
	return fmt.Errorf("%w: %s", ErrScript, detail)
}

// needsProcessCheck reports whether classifying err depends on whether
// Spotify is running, so the process list is consulted only then.
func needsProcessCheck(err error) bool {
	se, ok := errors.AsType[*ScriptError](err)
	return ok && ambiguousCode(se.Code)
}

// ambiguousCode reports error numbers that mean "not running" or "blocked"
// depending on whether the Spotify process exists.
func ambiguousCode(code int) bool {
	switch code {
	case codeProcNotFound, codeConnInvalid, codeLaunchServices, codeLaunchServices2:
		return true
	}
	return false
}
