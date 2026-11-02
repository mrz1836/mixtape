package spotify

import (
	"context"
	"os"
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
	assert.Contains(t, err.Error(), "boom")
	require.ErrorIs(t, Classify(err, true), ErrNotAuthorized, "the error number survives into the message")
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
