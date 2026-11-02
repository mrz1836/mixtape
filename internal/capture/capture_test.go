package capture

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseMethod(t *testing.T) {
	t.Parallel()
	tests := map[string]Method{
		"":         MethodAuto,
		"auto":     MethodAuto,
		" TAP ":    MethodTap,
		"Device":   MethodDevice,
		"device\n": MethodDevice,
	}
	for in, want := range tests {
		got, err := ParseMethod(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}

	_, err := ParseMethod("blackhole")
	require.ErrorIs(t, err, ErrUnknownMethod)
	assert.Contains(t, err.Error(), `"blackhole"`)
}

func TestSelect(t *testing.T) {
	t.Parallel()
	supported := TapStatus{Supported: true}
	unsupported := TapStatus{Reason: "macOS 13.6 is older than 14.2"}

	tests := map[string]struct {
		requested Method
		tap       TapStatus
		want      Method
		err       error
	}{
		"auto prefers the tap":          {MethodAuto, supported, MethodTap, nil},
		"empty is auto":                 {"", supported, MethodTap, nil},
		"auto falls back to the device": {MethodAuto, unsupported, MethodDevice, nil},
		"tap when supported":            {MethodTap, supported, MethodTap, nil},
		"tap when unsupported fails":    {MethodTap, unsupported, "", ErrTapUnsupported},
		"tap without a reason fails":    {MethodTap, TapStatus{}, "", ErrTapUnsupported},
		"device always works":           {MethodDevice, unsupported, MethodDevice, nil},
		"device even with a tap":        {MethodDevice, supported, MethodDevice, nil},
		"unknown method":                {Method("magic"), supported, "", ErrUnknownMethod},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := Select(tc.requested, tc.tap)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				assert.Empty(t, got)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	_, err := Select(MethodTap, unsupported)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "older than 14.2", "the reason reaches the user")
}

func TestFormat(t *testing.T) {
	t.Parallel()
	f := Format{Rate: 48000, Channels: 2}
	assert.Equal(t, 4, f.BytesPerFrame())
	assert.Equal(t, time.Second, f.Duration(48000))
	assert.Equal(t, 1500*time.Millisecond, f.Duration(72000))
	assert.Equal(t, int64(24000), f.Frames(500*time.Millisecond))
	assert.Equal(t, time.Duration(0), Format{}.Duration(100))
}
