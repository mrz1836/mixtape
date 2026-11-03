package cli

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/session"
)

// TestUserFacingMessages pins the exact text of common failures. The
// message is the error's own text, wrapped layer by layer: this keeps the
// wrapping readable as the code changes.
func TestUserFacingMessages(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		setup       func(h *harness)
		args        []string
		exit        int
		message     string
		hintContain string
	}{
		"another session holds the lock": {
			setup: func(h *harness) {
				h.world.addPlaylist("List", songs()...)
				lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 999, SessionID: "other"})
				require.NoError(h.t, err)
				h.t.Cleanup(func() { _ = lock.Release() })
			},
			args: []string{"record", plURL}, exit: ExitBusy,
			message:     "another mixtape recording session is running (session other, pid 999)",
			hintContain: "'mixtape stop' ends it",
		},
		"a link Spotify does not know": {
			args: []string{"record", plURL}, exit: ExitNotFound,
			message:     "https://open.spotify.com/playlist/" + plID + ": not found on Spotify",
			hintContain: "private playlists cannot be read",
		},
		"crossfade on": {
			setup: func(h *harness) {
				h.world.addPlaylist("List", songs()...)
				h.world.prefs.Crossfade, h.world.prefs.CrossfadeSeconds = true, 6.658
			},
			args: []string{"record", plURL}, exit: ExitNotReady,
			message:     "spotify crossfade is on, so songs overlap and cannot be cut cleanly (crossfade is set to 6.7s)",
			hintContain: `"Crossfade songs" and "Automix"`,
		},
		"an unknown setting": {
			args: []string{"config", "set", "volume", "11"}, exit: ExitUsage,
			message:     `unknown config key "volume" (known: library, capture, device, artwork, idle_timeout, max_failures, codec, aac_bitrate, bit_depth, copy_codec, output_rate)`,
			hintContain: "mixtape config show",
		},
		"a bad setting": {
			args: []string{"config", "set", "capture", "magic"}, exit: ExitUsage,
			message: `invalid config value: capture "magic" (want auto, tap or device)`,
		},
		"stop with nothing running": {
			args: []string{"stop"}, exit: ExitNotFound,
			message:     "no recording session is running",
			hintContain: "'mixtape status' shows the last session",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			require.Equal(t, tc.exit, h.run(tc.args...), h.stderr.String())
			e := h.errDoc()
			assert.Equal(t, tc.message, e["message"])
			if tc.hintContain != "" {
				assert.Contains(t, e["hint"], tc.hintContain)
			}
		})
	}
}
