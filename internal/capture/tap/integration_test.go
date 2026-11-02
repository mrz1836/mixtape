//go:build captureintegration && darwin && cgo

// These tests open a real process tap on Spotify. They trigger the macOS
// "record system audio" prompt for the host app the first time, so they
// run only on request, with Spotify playing:
//
//	MIXTAPE_LIVE_CAPTURE=1 go test -tags captureintegration -run Integration -v ./internal/capture/...
//
// The build tag alone is not enough: tools that enable every discovered
// tag (magex test) must never open live audio, so the environment variable
// is required too.
package tap

import (
	"context"
	"encoding/binary"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

func TestIntegrationSpotifyProcesses(t *testing.T) {
	t.Parallel()
	requireLive(t)
	procs, err := SpotifyProcesses()
	require.NoError(t, err)
	for _, p := range procs {
		t.Logf("process object %d pid %d bundle %s", p.ObjectID, p.PID, p.BundleID)
	}
}

func TestIntegrationTapRecords(t *testing.T) {
	t.Parallel()
	requireLive(t)
	src, err := Open(Options{Wait: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	f := src.Format()
	t.Logf("format %+v", f)
	require.Positive(t, f.Rate)

	var (
		mu     sync.Mutex
		frames int
		peak   int
		first  time.Time
		blocks int
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, src.Start(ctx, func(b capture.Block) {
		mu.Lock()
		defer mu.Unlock()
		if first.IsZero() {
			first = b.At
		}
		blocks++
		frames += b.Frames
		for i := 0; i+1 < len(b.PCM); i += 2 {
			v := int(int16(binary.LittleEndian.Uint16(b.PCM[i:])))
			peak = max(peak, v, -v)
		}
	}))
	go func() {
		for ev := range src.Events() {
			t.Logf("event %s: %s", ev.Kind, ev.Detail)
		}
	}()
	<-ctx.Done()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d blocks, %d frames (%.2fs), peak %d, first block %v ago", blocks, frames,
		float64(frames)/float64(f.Rate), peak, time.Since(first))
	assert.Positive(t, frames, "the tap delivers audio callbacks")
	if peak == 0 {
		t.Log("all zeros: Spotify paused, or the host app lacks the System Audio Recording permission")
	}
}

// requireLive skips unless live capture tests were asked for explicitly.
func requireLive(t *testing.T) {
	t.Helper()
	if os.Getenv("MIXTAPE_LIVE_CAPTURE") != "1" {
		t.Skip("live capture test: set MIXTAPE_LIVE_CAPTURE=1 (needs Spotify playing and the owner present)")
	}
}
