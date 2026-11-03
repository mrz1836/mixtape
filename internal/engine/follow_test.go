package engine

import (
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// playCommands lists the "play track" commands from the from'th command on.
func playCommands(s *sim, from int) []string {
	var out []string
	for _, c := range s.spotify.Commands()[from:] {
		if len(c) > 5 && c[:5] == "play " {
			out = append(out, c)
		}
	}
	return out
}

func playlistSpec(col spotify.Collection) library.Spec {
	return library.Spec{Kind: library.KindPlaylist, SpotifyID: col.Ref.ID}
}

func TestFollowWithoutAKnownListPlaysThePlaylistItself(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	// The embed page listed nothing: the playlist is followed from its top.
	pl := s.playlist("pl0000000000000000000z", "Unlisted", false)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, songB.id, songC.id}

	plan, res := s.run([]spotify.Collection{pl}, Options{IdleTimeout: time.Hour}, nil)
	requireComplete(t, res)
	assert.Equal(t, ModeFollow, plan.Mode)
	assert.Equal(t, 3, res.Counts.Recorded)
	plays := playCommands(s, 0)
	require.NotEmpty(t, plays)
	assert.Equal(t, "play "+pl.Ref.URI()+" in ", plays[0], "the playlist itself, with no song to start from")
	c := s.collection(playlistSpec(pl))
	for _, id := range []string{songA.id, songB.id, songC.id} {
		assert.True(t, c.Done(id), id)
	}
	assert.Less(t, s.now().Sub(s.start), time.Minute, "the loop ends the session, not the idle timeout")
}

func TestFollowStartsPastTheLastKnownSongWhenItDidNotJustPlay(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	_, res := s.run(singles(songB), Options{}, nil)
	requireComplete(t, res)

	s.reopen()
	s.resetEvents()
	start := len(s.spotify.Commands())
	// Bravo, the last listed song, is copied from Singles, so it does not
	// play to its end in the playlist: following asks for it in the
	// playlist's context and skips it as it begins.
	pl := s.playlist("pl0000000000000000000y", "Long", false, songA, songB)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, songB.id, songC.id}

	plan, res := s.run([]spotify.Collection{pl}, Options{IdleTimeout: time.Hour}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, plan.Count(ActionCopy))
	assert.Equal(t, 2, res.Counts.Recorded, "Alpha, and Charlie past the list")
	assert.Equal(t, 1, res.Counts.Copied)
	cmds := s.spotify.Commands()[start:]
	i := slices.Index(cmds, "play "+spotify.TrackURI(songB.id)+" in "+pl.Ref.URI())
	require.GreaterOrEqual(t, i, 0, "commands: %v", cmds)
	require.Less(t, i+1, len(cmds))
	assert.Equal(t, "next", cmds[i+1], "the known song is skipped as soon as it begins")
	assert.True(t, s.collection(playlistSpec(pl)).Done(songC.id))
}

// TestFollowMovesOnOnceFromASongPastTheListItHas covers Spotify reporting a
// song once more after mixtape moved on from it with next, for a song past
// the known list that the library already has: no new take of it.
func TestFollowMovesOnOnceFromASongPastTheListItHas(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	_, res := s.run(singles(songC), Options{}, nil)
	requireComplete(t, res)

	s.reopen()
	s.resetEvents()
	pl := s.playlist("pl0000000000000000000w", "Long", false, songA)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, songC.id, songB.id}
	s.spotify.user(func(f *fakeSpotify) { f.oldTrackOnNext = 1 })

	_, res = s.run([]spotify.Collection{pl}, Options{IdleTimeout: time.Hour}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Copied, "Charlie, past the list, is copied from Singles")
	assert.Equal(t, 2, res.Counts.Recorded, "Alpha and Bravo")
	assert.Zero(t, res.Counts.Discarded, "Charlie reported again is no take")
	c := s.collection(playlistSpec(pl))
	for _, id := range []string{songA.id, songB.id, songC.id} {
		assert.True(t, c.Done(id), id)
	}
}

func TestFollowEndsWhenThePlaylistDoesNotPlay(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	pl := s.playlist("pl0000000000000000000v", "Region Locked", false)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id}
	s.spotify.unplayable[songA.id] = true // Spotify plays nothing of it here

	_, res := s.run([]spotify.Collection{pl}, Options{StartTimeout: 2 * time.Second, IdleTimeout: time.Hour}, nil)
	require.NoError(t, res.Err)
	// A followed playlist that stopped has been followed to its end.
	assert.Equal(t, OutcomeComplete, res.Outcome)
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Less(t, s.now().Sub(s.start), time.Minute, "the start timeout ends it, not the idle timeout")
	assert.Equal(t, 1, s.spotify.plays(0), "the playlist is asked for once")
}

func TestFollowedPlaylistDoesNotAdoptTheSongPlayingBeforeIt(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	// The embed listed only Alpha; the album plays (with repeat on) right
	// before the playlist is followed.
	pl := s.playlist("pl00000000000000000003", "Long", false, songA)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, songB.id}
	album := spotify.Collection{
		Ref: spotify.Ref{Kind: spotify.KindAlbum, ID: "al00000000000000000003"}, Name: "Short", Owner: "Band",
		Source: spotify.SourceEmbed, Complete: true,
		Tracks: []spotify.CatalogTrack{{ID: songC.id, Title: songC.name, Artist: songC.artist, Duration: songC.dur, Playable: true, Position: 1}},
	}
	s.spotify.contexts[album.Ref.URI()] = []string{songC.id}

	_, res := s.run([]spotify.Collection{pl, album}, Options{IdleTimeout: time.Hour}, nil)
	requireComplete(t, res)
	c := s.collection(playlistSpec(pl))
	assert.True(t, c.Done(songB.id), "the song past the list is recorded")
	assert.Nil(t, c.Entry(songC.id), "the album's song is no song of the playlist")
	assert.Less(t, s.now().Sub(s.start), 5*time.Minute, "the loop is found: no hour of pressing next")
}

func TestTwoTruncatedPlaylistsAreFollowedOneAfterTheOther(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC, songD)
	// Each embed listed only its first song.
	p1 := s.playlist("pl00000000000000000001", "First", false, songA)
	s.spotify.contexts[p1.Ref.URI()] = []string{songA.id, songB.id}
	p2 := s.playlist("pl00000000000000000002", "Second", false, songC)
	s.spotify.contexts[p2.Ref.URI()] = []string{songC.id, songD.id}

	_, res := s.run([]spotify.Collection{p1, p2}, Options{IdleTimeout: time.Hour}, nil)
	requireComplete(t, res)
	assert.Equal(t, 4, res.Counts.Recorded)
	assert.True(t, s.collection(playlistSpec(p1)).Done(songB.id), "Bravo, past the first list")
	assert.True(t, s.collection(playlistSpec(p2)).Done(songD.id), "Delta, past the second list")
}
