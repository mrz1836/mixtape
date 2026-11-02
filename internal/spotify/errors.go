package spotify

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
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

// errCodeRe finds the trailing "(-1743)" osascript appends to an error.
var errCodeRe = regexp.MustCompile(`\((-?\d+)\)\s*$`)

// Classify maps an osascript failure to one of the package's sentinels,
// keeping the original text as detail. running says whether the Spotify
// process exists; it separates "not running" from "Apple Events blocked"
// (a sandbox) for the errors that look alike. Errors that already carry a
// sentinel pass through unchanged.
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
	code, ok := errorCode(detail)
	if !ok {
		return fmt.Errorf("%w: %s", ErrScript, detail)
	}
	return classifyCode(code, detail, running)
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

// errorCode extracts the trailing error number from osascript output.
func errorCode(s string) (int, bool) {
	m := errCodeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, false
	}
	return n, true
}

// needsProcessCheck reports whether classifying err depends on whether
// Spotify is running, so the process list is consulted only then.
func needsProcessCheck(err error) bool {
	code, ok := errorCode(strings.TrimSpace(err.Error()))
	return ok && ambiguousCode(code)
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
