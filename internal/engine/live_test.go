package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
