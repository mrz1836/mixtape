//go:build captureintegration && darwin && cgo

// This test opens a real capture device. It triggers the macOS microphone
// prompt for the host app the first time, so it runs only on request:
//
//	MIXTAPE_LIVE_CAPTURE=1 MIXTAPE_TEST_DEVICE=BlackHole go test -tags captureintegration -run Integration -v ./internal/capture/device/
//
// The build tag alone is not enough: tools that enable every discovered
// tag (magex test) must never open live audio, so the environment variable
// is required too.
package device

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
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestIntegrationDeviceRecords(t *testing.T) {
	t.Parallel()
	testutil.RequireLive(t)
	name := os.Getenv("MIXTAPE_TEST_DEVICE")
	if name == "" {
		name = "BlackHole"
	}
	src, err := Open(name)
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
		for i := 0; i+1 < len(b.PCM); i += 2 {
			v := int(int16(binary.LittleEndian.Uint16(b.PCM[i:])))
			peak = max(peak, v, -v)
		}
	}))
	<-ctx.Done()
	mu.Lock()
	defer mu.Unlock()
	t.Logf("%d frames (%.2fs), peak %d, largest block gap %v", frames, float64(frames)/float64(f.Rate), peak, maxGap)
	assert.Positive(t, frames)
}
