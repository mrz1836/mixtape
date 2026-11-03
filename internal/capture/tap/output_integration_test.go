//go:build captureintegration && darwin && cgo

package tap

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

// envLiveOutputRate enables the test that switches the speakers' rate:
// on top of MIXTAPE_LIVE_CAPTURE, because it changes a system setting
// (and puts it back).
const envLiveOutputRate = "MIXTAPE_LIVE_OUTPUT_RATE"

// TestIntegrationOutputRate switches the default output to another rate
// it offers and back, as a session with output_rate does. It does not run
// in parallel: it changes what every other test would hear.
func TestIntegrationOutputRate(t *testing.T) { //nolint:paralleltest // switches the machine's output rate
	if os.Getenv(envLiveOutputRate) != "1" {
		t.Skip("switches the output device's rate: set " + envLiveOutputRate + "=1 (with the owner present)")
	}
	ctl := OutputControl()
	dev, err := ctl.DefaultOutput()
	require.NoError(t, err)
	target := 44100
	if dev.Rate == target {
		target = 48000
	}
	if !dev.Supports(target) {
		t.Skipf("%s offers no other rate to switch to (%s)", dev.Name, dev.RateList())
	}
	t.Cleanup(func() {
		if err := ctl.SetOutputRate(context.Background(), dev.UID, dev.Rate); err != nil {
			t.Errorf("could not put %s back to %d Hz: %v", dev.Name, dev.Rate, err)
		}
	})
	t.Logf("%s: %d Hz → %d Hz", dev.Name, dev.Rate, target)

	err = ctl.SetOutputRate(t.Context(), dev.UID, target)
	if errors.Is(err, capture.ErrOutputReverted) {
		t.Skipf("another app holds %s at its rate: %v", dev.Name, err)
	}
	require.NoError(t, err)
	got, err := ctl.OutputRate(dev.UID)
	require.NoError(t, err)
	assert.Equal(t, target, got)

	var warns []string
	sw := &capture.OutputSwitch{Control: ctl, Warn: func(m string) { warns = append(warns, m) }}
	require.NoError(t, ctl.SetOutputRate(t.Context(), dev.UID, dev.Rate))
	require.True(t, sw.Begin(t.Context(), target, nil), "%v", warns)
	got, err = ctl.OutputRate(dev.UID)
	require.NoError(t, err)
	assert.Equal(t, target, got)
	sw.End(t.Context())
	got, err = ctl.OutputRate(dev.UID)
	require.NoError(t, err)
	assert.Equal(t, dev.Rate, got, "End puts the original back")
	assert.Empty(t, warns)
}
