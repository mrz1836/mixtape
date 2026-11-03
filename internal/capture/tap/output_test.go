//go:build darwin && cgo

package tap

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

// Reading the default output changes nothing, so it runs everywhere it
// can (a sandbox hides the devices: then it skips).
func TestOutputControlReadsTheDefaultOutput(t *testing.T) {
	t.Parallel()
	ctl := OutputControl()
	dev, err := ctl.DefaultOutput()
	if err != nil {
		t.Skipf("no default output to read here: %v", err)
	}
	t.Logf("%s (%s) at %d Hz, offers %s", dev.Name, dev.UID, dev.Rate, dev.RateList())
	assert.NotEmpty(t, dev.UID)
	assert.Positive(t, dev.Rate)
	if len(dev.Rates) > 0 {
		assert.True(t, dev.Supports(dev.Rate), "a device offers the rate it runs at")
	}
	rate, err := ctl.OutputRate(dev.UID)
	require.NoError(t, err)
	assert.Equal(t, dev.Rate, rate)

	_, err = ctl.OutputRate("no-such-device-7f3a")
	require.ErrorIs(t, err, capture.ErrOutputGone)
	require.ErrorIs(t, ctl.SetOutputRate(t.Context(), "no-such-device-7f3a", 44100), capture.ErrOutputGone)
}

func TestHz(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 44100, hz(44099.9999))
	assert.Equal(t, 48000, hz(48000))
}
