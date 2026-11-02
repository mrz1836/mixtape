package spotify

import (
	"context"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// textError is an error with fixed text, standing in for osascript output.
type textError string

func (e textError) Error() string { return string(e) }

func nan() float64 { return math.NaN() }
func inf() float64 { return math.Inf(1) }

func TestClassify(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		err     error
		running bool
		want    error
	}{
		"denied":            {textError("Not authorized to send Apple events to Spotify. (-1743)"), true, ErrNotAuthorized},
		"event timeout":     {textError("AppleEvent timed out. (-1712)"), true, ErrPromptPending},
		"deadline":          {fmt.Errorf("osascript: %w", context.DeadlineExceeded), true, ErrPromptPending},
		"-600 stopped":      {textError("Application isn't running. (-600)"), false, ErrNotRunning},
		"-600 running":      {textError("Application isn't running. (-600)"), true, ErrAppleEventsBlocked},
		"-609 running":      {textError("Connection is invalid. (-609)"), true, ErrAppleEventsBlocked},
		"-10810 stopped":    {textError("An error of type -10810 has occurred. (-10810)"), false, ErrNotRunning},
		"-10822 running":    {textError("kLSServerCommunicationErr (-10822)\n"), true, ErrAppleEventsBlocked},
		"other code":        {textError("Can't get current track. (-1728)"), true, ErrScript},
		"no code":           {textError("exit status 1"), true, ErrScript},
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
	err := Classify(textError("Not authorized to send Apple events to Spotify. (-1743)"), false)
	assert.Contains(t, err.Error(), "Not authorized to send Apple events")
}

func TestNeedsProcessCheck(t *testing.T) {
	t.Parallel()
	assert.True(t, needsProcessCheck(textError("x (-600)")))
	assert.True(t, needsProcessCheck(textError("x (-10810)")))
	assert.False(t, needsProcessCheck(textError("x (-1743)")))
	assert.False(t, needsProcessCheck(textError("no code")))
}
