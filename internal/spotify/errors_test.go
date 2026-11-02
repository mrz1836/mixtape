package spotify

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textError is an error with fixed text, standing in for failures that are
// not AppleScript errors (osascript failing to start, a plain exit status).
type textError string

func (e textError) Error() string { return string(e) }

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		err     error
		running bool
		want    error
	}{
		"denied":            {&ScriptError{Code: -1743, Msg: "Not authorized to send Apple events to Spotify. (-1743)"}, true, ErrNotAuthorized},
		"event timeout":     {&ScriptError{Code: -1712, Msg: "AppleEvent timed out. (-1712)"}, true, ErrPromptPending},
		"deadline":          {fmt.Errorf("osascript: %w", context.DeadlineExceeded), true, ErrPromptPending},
		"-600 stopped":      {&ScriptError{Code: -600, Msg: "Application isn't running. (-600)"}, false, ErrNotRunning},
		"-600 running":      {&ScriptError{Code: -600, Msg: "Application isn't running. (-600)"}, true, ErrAppleEventsBlocked},
		"-609 running":      {&ScriptError{Code: -609, Msg: "Connection is invalid. (-609)"}, true, ErrAppleEventsBlocked},
		"-10810 stopped":    {&ScriptError{Code: -10810, Msg: "An error of type -10810 has occurred. (-10810)"}, false, ErrNotRunning},
		"-10822 running":    {&ScriptError{Code: -10822, Msg: "kLSServerCommunicationErr (-10822)\n"}, true, ErrAppleEventsBlocked},
		"other code":        {&ScriptError{Code: -1728, Msg: "Can't get current track. (-1728)"}, true, ErrScript},
		"no code":           {&ScriptError{Msg: "osascript: signal: killed: "}, true, ErrScript},
		"wrapped":           {fmt.Errorf("watch: %w", &ScriptError{Code: -1743, Msg: "denied (-1743)"}), true, ErrNotAuthorized},
		"not a script":      {textError("exit status 1"), true, ErrScript},
		"number in text":    {textError("Not authorized to send Apple events to Spotify. (-1743)"), true, ErrScript},
		"already sentinel":  {fmt.Errorf("x: %w", ErrNotRunning), true, ErrNotRunning},
		"already authorize": {ErrNotAuthorized, false, ErrNotAuthorized},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := Classify(tc.err, tc.running)
			require.ErrorIs(t, got, tc.want)
		})
	}
	require.NoError(t, Classify(nil, true))
}

func TestClassifyKeepsDetail(t *testing.T) {
	t.Parallel()
	err := Classify(&ScriptError{Code: -1743, Msg: "Not authorized to send Apple events to Spotify. (-1743)"}, false)
	require.ErrorIs(t, err, ErrNotAuthorized)
	assert.Equal(t, ErrNotAuthorized.Error()+": Not authorized to send Apple events to Spotify. (-1743)", err.Error())
}

func TestNeedsProcessCheck(t *testing.T) {
	t.Parallel()
	assert.True(t, needsProcessCheck(&ScriptError{Code: -600, Msg: "x (-600)"}))
	assert.True(t, needsProcessCheck(fmt.Errorf("y: %w", &ScriptError{Code: -10810, Msg: "x (-10810)"})))
	assert.False(t, needsProcessCheck(&ScriptError{Code: -1743, Msg: "x (-1743)"}))
	assert.False(t, needsProcessCheck(&ScriptError{Msg: "no code"}))
	assert.False(t, needsProcessCheck(textError("x (-600)")), "only a ScriptError carries a number")
}
