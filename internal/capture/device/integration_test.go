//go:build captureintegration && darwin && cgo

// This test opens a real capture device (MIXTAPE_TEST_DEVICE, default
// BlackHole). It triggers the macOS microphone prompt for the host app the
// first time, so it runs only on request:
//
//	MIXTAPE_LIVE_CAPTURE=1 magex test
//
// magex test enables every build tag it finds, captureintegration
// included, so the tag alone must stay harmless: the environment variable
// is what opens live audio.
package device

import (
	"context"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestIntegrationDeviceRecords(t *testing.T) {
	t.Parallel()
	testutil.RequireLive(t)
	name := os.Getenv("MIXTAPE_TEST_DEVICE")
	if name == "" {
		name = "BlackHole"
	}
	src, err := Open(name, 24)
	require.NoError(t, err)
	t.Cleanup(func() { _ = src.Close() })
	f := src.Format()
	t.Logf("%s: format %+v", src.Name(), f)

	var (
		mu           sync.Mutex
		frames, peak int
		lastAt       time.Time
		maxGap       time.Duration
	)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	require.NoError(t, src.Start(ctx, func(b capture.Block) {
		mu.Lock()
		defer mu.Unlock()
		if !lastAt.IsZero() {
			maxGap = max(maxGap, b.At.Sub(lastAt))
		}
		lastAt = b.At
		frames += b.Frames
		assert.Len(t, b.PCM, b.Frames*f.BytesPerFrame())
		peak = max(peak, capture.Peak(b.PCM, f))
	}))
	<-ctx.Done()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d frames (%.2fs), peak %d (16-bit scale), largest block gap %v", frames, float64(frames)/float64(f.Rate), peak, maxGap)
	assert.Equal(t, 24, f.SampleBits())
	assert.Positive(t, frames)
}
