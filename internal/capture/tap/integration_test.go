//go:build captureintegration && darwin && cgo

// These tests open a real process tap on Spotify. The host app (the
// terminal that runs them) needs the System Audio Recording permission, so
// they run only on request, with Spotify playing:
//
//	MIXTAPE_LIVE_CAPTURE=1 magex test
//
// magex test enables every build tag it finds, captureintegration
// included, so the tag alone must stay harmless: the environment variable
// is what opens live audio.
package tap

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestIntegrationSpotifyProcesses(t *testing.T) {
	t.Parallel()
	testutil.RequireLive(t)
	procs, err := SpotifyProcesses()
	require.NoError(t, err)
	for _, p := range procs {
		t.Logf("process object %d pid %d bundle %s", p.ObjectID, p.PID, p.BundleID)
	}
}

func TestIntegrationTapRecords(t *testing.T) {
	t.Parallel()
	testutil.RequireLive(t)
	for _, bits := range []int{16, 24} {
		t.Run(strconv.Itoa(bits)+"-bit", func(t *testing.T) { tapRecords(t, bits) })
	}
}

// tapRecords taps Spotify for three seconds at bits bits a sample. At 24
// bits it also counts samples whose low byte is busy: the detail a 16-bit
// capture drops.
func tapRecords(t *testing.T, bits int) {
	src, err := Open(t.Context(), Options{Wait: 5 * time.Second, Bits: bits})
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	f := src.Format()
	t.Logf("format %+v", f)
	require.Positive(t, f.Rate)
	require.Equal(t, bits, f.SampleBits())

	var (
		mu       sync.Mutex
		frames   int
		peak     int
		lowBytes int
		first    time.Time
		blocks   int
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
		require.Len(t, b.PCM, b.Frames*f.BytesPerFrame())
		peak = max(peak, capture.Peak(b.PCM, f))
		if bits == 24 {
			for i := 0; i+2 < len(b.PCM); i += 3 {
				if b.PCM[i] != 0 {
					lowBytes++
				}
			}
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
	t.Logf("%d blocks, %d frames (%.2fs), peak %d (16-bit scale), %d samples with a busy low byte, first block %v ago",
		blocks, frames, float64(frames)/float64(f.Rate), peak, lowBytes, time.Since(first))
	assert.Positive(t, frames, "the tap delivers audio callbacks")
	if peak == 0 {
		t.Log("all zeros: Spotify paused, or the host app lacks the System Audio Recording permission")
	}
}
