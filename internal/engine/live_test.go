package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

func TestLiveLimitEndsComplete(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	s.listenerPlays(false, songA.id, songB.id, songC.id)
	_, res := s.run(nil, Options{Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Zero(t, res.Counts.Remaining, "the song playing as the limit is reached is the listener's")
	assert.Nil(t, s.collection(singlesSpec()).Entry(songB.id), "and leaves nothing pending behind")
}

// TestLiveModeMovesOnOnceFromASongItHas covers Spotify reporting the song
// mixtape just moved it off once more after "next": that is no new take of
// the song, and no reason to press next again (which would skip the next
// song). Seen in CI with the copy, where it left Bravo unrecorded.
func TestLiveModeMovesOnOnceFromASongItHas(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		// earlier records Alpha before the live session: in a playlist
		// (complete elsewhere, so copied) or as a single (already here).
		earlier func(s *sim) []spotify.Collection
		check   func(t *testing.T, c session.Counts)
	}{
		"complete elsewhere": {
			earlier: func(s *sim) []spotify.Collection {
				return []spotify.Collection{s.playlist("pl0000000000000000000l", "Earlier", true, songA)}
			},
			check: func(t *testing.T, c session.Counts) { assert.Equal(t, 1, c.Copied) },
		},
		"already in the folder": {
			earlier: func(*sim) []spotify.Collection { return singles(songA) },
			check:   func(t *testing.T, c session.Counts) { assert.Equal(t, 1, c.Skipped) },
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB)
			_, res := s.run(tc.earlier(s), Options{}, nil)
			requireComplete(t, res)

			s.reopen()
			s.resetEvents()
			start := len(s.spotify.Commands())
			s.spotify.user(func(f *fakeSpotify) {
				f.queue, f.idx, f.pos, f.playing = []string{songA.id, songB.id}, 0, 0, true
				f.oldTrackOnNext = 1
			})
			_, res = s.run(nil, Options{Limit: 1}, nil)
			requireComplete(t, res)
			tc.check(t, res.Counts)
			assert.Equal(t, 1, res.Counts.Recorded)
			assert.Zero(t, res.Counts.Discarded, "Alpha reported again is no take")
			assert.True(t, s.collection(singlesSpec()).Done(songB.id))
			nexts := 0
			for _, c := range s.spotify.Commands()[start:] {
				if c == "next" {
					nexts++
				}
			}
			assert.Equal(t, 1, nexts, "one next moves Spotify on from Alpha")
		})
	}
}

func TestLiveSongCutShortIsRecordedWhenPlayedAgain(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.listenerPlays(true, songA.id, songB.id)
	// The listener jumps to Bravo three seconds into Alpha; with repeat on,
	// Alpha comes round again and plays from its top.
	s.at[3*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.idx, f.pos = 1, 0 }) }
	_, res := s.run(nil, Options{IdleTimeout: 20 * time.Second}, nil)
	require.NoError(t, res.Err)
	assert.True(t, s.collection(singlesSpec()).Done(songA.id), "Alpha played through from its top the second time")
	assert.Equal(t, 1, res.Counts.Discarded, "the cut-short take")
}
