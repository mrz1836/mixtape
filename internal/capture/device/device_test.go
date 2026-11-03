//go:build darwin && cgo

package device

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestListEnumerates only enumerates devices: it never opens one, so it
// needs no permission and triggers no prompt.
func TestListEnumerates(t *testing.T) {
	t.Parallel()
	devices, err := List()
	if err != nil {
		t.Skipf("Core Audio enumeration unavailable here: %v", err)
	}
	for _, d := range devices {
		assert.NotEmpty(t, d.ID)
		assert.True(t, d.Input || d.Output, "%s is neither input nor output", d.Name)
		assert.False(t, d.DefaultInput && !d.Input, "%s: default input but not an input", d.Name)
		assert.False(t, d.DefaultOutput && !d.Output, "%s: default output but not an output", d.Name)
	}
	ids := map[string]bool{}
	for _, d := range devices {
		assert.False(t, ids[d.ID], "device %s listed twice", d.ID)
		ids[d.ID] = true
	}
}

func TestOpenUnknownDeviceListsAvailable(t *testing.T) {
	t.Parallel()
	if _, err := List(); err != nil {
		t.Skipf("Core Audio enumeration unavailable here: %v", err)
	}
	_, err := Open("no-such-device-ever-7f3a", 24)
	require.ErrorIs(t, err, ErrNoDevice)
	assert.Contains(t, err.Error(), "available capture devices")
}
