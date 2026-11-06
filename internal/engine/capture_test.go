package engine

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/spool"
)

// spoolNow is the spool the engine writes to at the moment.
func (s *sim) spoolNow() *spool.Spool {
	s.eng.spoolMu.Lock()
	defer s.eng.spoolMu.Unlock()
	return s.eng.spool
}

func TestCaptureEventsDiscardTheSongTheyTouch(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		ev       capture.Event
		reason   string
		newSpool bool
	}{
		"an overflow keeps the spool": {
			ev:     capture.Event{Kind: capture.EventOverflow, Detail: "dropped 512 frames"},
			reason: "capture problem (overflow): dropped 512 frames",
		},
		"a device change in the same format keeps the spool": {
			ev:     capture.Event{Kind: capture.EventDeviceChanged},
			reason: "capture problem (device_changed)",
		},
		"a format change opens a new spool": {
			ev:       capture.Event{Kind: capture.EventFormatChanged, Detail: "2 channels"},
			reason:   "capture problem (format_changed): 2 channels",
			newSpool: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA)
			var before, after *spool.Spool
			s.at[2*time.Second] = func(s *sim) {
				before = s.spoolNow()
				s.source.events <- tc.ev
			}
			s.at[4*time.Second] = func(s *sim) { after = s.spoolNow() }

			_, res := s.run(singles(songA), Options{}, nil)
			requireComplete(t, res)
			assert.Equal(t, 1, res.Counts.Recorded, "the song is recorded again from the top")
			discarded := s.eventsOf(EventTrackDiscarded)
			require.Len(t, discarded, 1)
			assert.Equal(t, tc.reason, discarded[0].Reason)
			assert.Contains(t, messages(s.eventsOf(EventWarning)), tc.reason)
			require.NotNil(t, before)
			require.NotNil(t, after)
			assert.Equal(t, tc.newSpool, before != after)
		})
	}
}

func TestCaptureStoppedNamesWhy(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.at[2*time.Second] = func(s *sim) {
		s.source.events <- capture.Event{Kind: capture.EventStopped, Detail: "the device was unplugged"}
	}

	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrCaptureStopped)
	assert.Equal(t, ErrCaptureStopped.Error()+": the device was unplugged", res.Err.Error())
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Remaining)
}

func TestSpoolThatCannotBeReopenedEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.at[2*time.Second] = func(s *sim) {
		// The spool folder turns into a file, so the new spool a format
		// change needs cannot be created.
		dir := filepath.Join(s.home, "spool")
		if err := os.RemoveAll(dir); err != nil {
			s.actionFailed(err)
			return
		}
		if err := os.WriteFile(dir, nil, 0o600); err != nil {
			s.actionFailed(err)
			return
		}
		s.source.events <- capture.Event{Kind: capture.EventFormatChanged}
	}

	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorContains(t, res.Err, "reopen spool")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Remaining, "the song stays pending for the next run")
	discarded := s.eventsOf(EventTrackDiscarded)
	require.Len(t, discarded, 1)
	assert.Equal(t, "capture problem (format_changed)", discarded[0].Reason)
}

func TestSilentTapEndsTheSessionAsMissingPermission(t *testing.T) {
	t.Parallel()
	long := simTrack{id: "0000000000000000000ttt", name: "Long", artist: "X", dur: 30 * time.Second}
	s := newSim(t, long)
	s.source.silent, s.source.method = true, capture.MethodTap

	_, res := s.run(singles(long), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrSilentTap, "a silent tap means macOS withholds the audio")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
}

// tapSim is a session whose capture behaves like the process tap: audio
// arrives only while Spotify plays on this Mac.
func tapSim(t *testing.T, tracks ...simTrack) *sim {
	t.Helper()
	s := newSim(t, tracks...)
	s.source.method, s.source.autostart = capture.MethodTap, true
	return s
}

func TestSpotifyPlayingOnAnotherDeviceEndsATapSessionAtOnce(t *testing.T) {
	t.Parallel()
	long := longSong("0000000000000000000eee", 30*time.Second)
	s := tapSim(t, long)
	s.spotify.elsewhere = true // Spotify Connect: a speaker plays it, this Mac sends nothing

	_, res := s.run(singles(long), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrPlaysElsewhere)
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Remaining, "the song stays pending for the next run")
	discarded := s.eventsOf(EventTrackDiscarded)
	require.Len(t, discarded, 1)
	assert.Less(t, discarded[0].Time.Sub(s.start), silenceWindow, "it ends long before the silence watchdog would")
	cmds := s.spotify.Commands()
	assert.Equal(t, "pause", cmds[len(cmds)-1], "the other device is paused, not left playing")
}

func TestCaptureDeviceStillHearsSilenceWhenSpotifyPlaysElsewhere(t *testing.T) {
	t.Parallel()
	long := longSong("0000000000000000000ddd", 30*time.Second)
	s := newSim(t, long)
	s.spotify.elsewhere = true // a device receives audio regardless: only its silence tells

	_, res := s.run(singles(long), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrSilentDevice)
}

func TestMuteRaisesTheVolumeOnlyOnceSpotifyIsHeardOnThisMac(t *testing.T) {
	t.Parallel()
	s := tapSim(t, songA) // the volume at 50

	_, res := s.run(singles(songA), Options{Mute: true}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded), "the take at the old volume is let go, not discarded")
	saved := s.eventsOf(EventTrackSaved)
	require.Len(t, saved, 1)
	assert.Equal(t, 1, saved[0].Track.Attempt, "the restart is no attempt")
	cmds := s.spotify.Commands()
	play := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "play ") })
	volume, seek := slices.Index(cmds, "volume"), slices.Index(cmds, "seek")
	assert.True(t, play >= 0 && volume > play && seek > volume,
		"played, heard on this Mac, turned up, then started over from the top: %v", cmds)
	assert.Equal(t, 50, s.spotify.volume, "the volume is put back at the end")
}

func TestMuteTurnsUpWithoutARestartWhenSpotifyIsHeardBeforeTheFirstSong(t *testing.T) {
	t.Parallel()
	s := tapSim(t, songA, songB)
	// The listener's music already plays on this Mac: the tap hears it at once.
	s.spotify.queue, s.spotify.idx, s.spotify.playing = []string{songB.id}, 0, true

	_, res := s.run(singles(songA), Options{Mute: true}, nil)
	requireComplete(t, res)
	cmds := s.spotify.Commands()
	play := slices.IndexFunc(cmds, func(c string) bool { return strings.HasPrefix(c, "play ") })
	volume := slices.Index(cmds, "volume")
	assert.True(t, volume >= 0 && volume < play, "turned up before the first song: %v", cmds)
	assert.NotContains(t, cmds, "seek", "nothing recorded at the old volume, so nothing starts over")
}

func TestMuteNeverTurnsUpSpotifyPlayingOnAnotherDevice(t *testing.T) {
	t.Parallel()
	long := longSong("0000000000000000000mmm", 30*time.Second)
	s := tapSim(t, long)
	s.spotify.elsewhere = true // casting to a speaker, which --mute cannot silence

	_, res := s.run(singles(long), Options{Mute: true}, nil)
	require.ErrorIs(t, res.Err, ErrPlaysElsewhere)
	assert.NotContains(t, s.spotify.Commands(), "volume", "the speaker keeps the listener's volume")
	assert.Equal(t, 50, s.spotify.volume)
}

func TestPeakCountsNegativeSamplesByTheirMagnitude(t *testing.T) {
	t.Parallel()
	pcm := func(samples ...int16) []byte {
		b := make([]byte, 2*len(samples))
		for i, v := range samples {
			binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
		}
		return b
	}
	// Audio swings both ways: a quiet positive half must not hide a loud
	// negative one from the silence check.
	f := capture.Format{Rate: testRate, Channels: 2}
	assert.Equal(t, 3000, capture.Peak(pcm(100, -3000, 20), f))
	assert.Equal(t, 1<<15, capture.Peak(pcm(math.MinInt16), f))
	assert.Equal(t, math.MaxInt16, capture.Peak(pcm(math.MaxInt16, -5), f))
	assert.Zero(t, capture.Peak(pcm(), f))
	assert.Zero(t, capture.Peak([]byte{0xff}, f), "a trailing odd byte is not a sample")
}
