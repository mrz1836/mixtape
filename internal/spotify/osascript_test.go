package spotify

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests run the real osascript with scripts that never address an
// application, so they exercise process handling without touching Spotify.

func requireOSAScript(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(osascriptPath); err != nil {
		t.Skip("osascript not available (macOS only)")
	}
}

func TestOSAScriptRun(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{}
	out, err := r.Run(t.Context(), "on run argv\nreturn \"hi \" & (item 1 of argv)\nend run", "there")
	require.NoError(t, err)
	assert.Equal(t, "hi there", out)
}

func TestOSAScriptRunError(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{}
	_, err := r.Run(t.Context(), `error "boom" number -1743`)
	require.Error(t, err)
	se, ok := errors.AsType[*ScriptError](err)
	require.True(t, ok, "a failed script is a ScriptError: %v", err)
	assert.Equal(t, -1743, se.Code)
	assert.True(t, strings.HasPrefix(err.Error(), "osascript: exit status 1: "), err.Error())
	assert.True(t, strings.HasSuffix(err.Error(), "boom (-1743)"), "the message keeps osascript's text: %v", err)
	require.ErrorIs(t, Classify(err, true), ErrNotAuthorized)
}

func TestOSAScriptRunTimeout(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{Timeout: 100 * time.Millisecond}
	_, err := r.Run(t.Context(), "delay 5")
	require.ErrorIs(t, Classify(err, true), ErrPromptPending)
}

func TestOSAScriptStart(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{}
	script := "on run argv\nrepeat with i from 1 to (item 1 of argv) as integer\nlog \"line \" & i\nend repeat\nend run"
	ch, err := r.Start(t.Context(), script, "3")
	require.NoError(t, err)

	var texts []string
	var last Line
	for l := range ch {
		if l.Err != nil {
			last = l
			continue
		}
		if isSnapshotLine(l.Text) {
			t.Fatalf("unexpected marker line %q", l.Text)
		}
		if len(l.Text) >= 4 && l.Text[:4] == "line" {
			texts = append(texts, l.Text)
		}
		assert.False(t, l.At.IsZero())
	}
	assert.Equal(t, []string{"line 1", "line 2", "line 3"}, texts)
	require.ErrorIs(t, last.Err, errNoOutput)
}

func TestOSAScriptStartScriptError(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{}
	ch, err := r.Start(t.Context(), "on run argv\nlog \"working\"\nerror \"boom\" number -1743\nend run")
	require.NoError(t, err)
	var last Line
	for l := range ch {
		last = l
	}
	se, ok := errors.AsType[*ScriptError](last.Err)
	require.True(t, ok, "the exit of a failed script is a ScriptError: %v", last.Err)
	assert.Equal(t, -1743, se.Code)
	assert.Contains(t, se.Msg, "boom")
	require.ErrorIs(t, Classify(last.Err, true), ErrNotAuthorized)
}

func TestExitError(t *testing.T) {
	t.Parallel()
	exit := textError("exit status 1")

	err := exitError(exit, "32:38: execution error: boom (-1743)")
	se, ok := errors.AsType[*ScriptError](err)
	require.True(t, ok)
	assert.Equal(t, &ScriptError{Code: -1743, Msg: "osascript: exit status 1: 32:38: execution error: boom (-1743)"}, se)

	err = exitError(exit, "2026-11-02 osascript[1:2] Connection Invalid error for service")
	_, ok = errors.AsType[*ScriptError](err)
	assert.False(t, ok, "a last line without a number may be log noise")
	require.ErrorIs(t, err, exit)
	assert.Equal(t, "osascript: exit status 1: 2026-11-02 osascript[1:2] Connection Invalid error for service", err.Error())
}

func TestErrorCode(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]int{
		"execution error: boom (-1743)":        -1743,
		"kLSServerCommunicationErr (-10822)\n": -10822,
		"positive (42)":                        42,
	} {
		got, ok := errorCode(s)
		assert.True(t, ok, s)
		assert.Equal(t, want, got, s)
	}
	for _, s := range []string{"", "exit status 1", "(-1743) then more", "(99999999999999999999)"} {
		_, ok := errorCode(s)
		assert.False(t, ok, s)
	}
}

func TestOSAScriptStartCancel(t *testing.T) {
	t.Parallel()
	requireOSAScript(t)
	r := &OSAScript{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	ch, err := r.Start(ctx, "repeat\ndelay 0.05\nlog \"tick\"\nend repeat")
	require.NoError(t, err)
	for l := range ch {
		if l.Text == "tick" {
			break // the process is up
		}
	}
	cancel()
	done := make(chan struct{})
	go func() {
		for l := range ch {
			_ = l // drain until the runner closes the channel
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not stop after cancel")
	}
}

func TestStreamLinesSurvivesAnOverlongLine(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", maxLineBytes+100)
	r := strings.NewReader("first\r\n" + long + "\nS\x1fsnapshot\nlast without newline")
	out := make(chan Line, 8)
	last := streamLines(t.Context(), r, out)
	close(out)
	var got []string
	for l := range out {
		got = append(got, l.Text)
	}
	require.Len(t, got, 4, "the lines after the long one still arrive")
	assert.Equal(t, "first", got[0])
	assert.Len(t, got[1], maxLineBytes, "the long line is cut")
	assert.Equal(t, "S\x1fsnapshot", got[2])
	assert.Equal(t, "last without newline", last)
	assert.Zero(t, r.Len(), "the pipe is read to its end")
}
