package engine

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// skipAsRoot skips tests that rely on read-only folders refusing writes.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
}

// readOnlyAt makes dir read-only when the session reaches offset, and
// writable again when the test ends.
func (s *sim) readOnlyAt(offset time.Duration, dir string) {
	s.t.Helper()
	s.t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	s.at[offset] = func(s *sim) {
		if err := os.Chmod(dir, 0o500); err != nil {
			s.actionFailed(err)
		}
	}
}

// listenerPlays has the listener start playing queue a second in, once
// capture runs: a song already playing when the session starts would be
// joined late.
func (s *sim) listenerPlays(repeat bool, queue ...string) {
	s.at[time.Second] = func(s *sim) {
		s.spotify.user(func(f *fakeSpotify) { f.queue, f.idx, f.pos, f.playing, f.repeat = queue, 0, 0, true, repeat })
	}
}

// hasPrefix reports whether any of msgs starts with prefix.
func hasPrefix(msgs []string, prefix string) bool {
	return slices.ContainsFunc(msgs, func(m string) bool { return strings.HasPrefix(m, prefix) })
}

func TestLiveModeCopiesASongCompleteElsewhereAndMovesOn(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	pl := s.playlist("pl0000000000000000000l", "Earlier", true, songA)
	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	requireComplete(t, res)

	s.reopen()
	s.resetEvents()
	start := len(s.spotify.Commands())
	s.spotify.user(func(f *fakeSpotify) {
		f.queue, f.idx, f.pos, f.playing = []string{songA.id, songB.id}, 0, 0, true
	})
	_, res = s.run(nil, Options{Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Copied)
	assert.Equal(t, 1, res.Counts.Recorded)
	copied := s.eventsOf(EventTrackCopied)
	require.Len(t, copied, 1)
	assert.Equal(t, songA.id, copied[0].Track.ID)
	from := s.collection(playlistSpec(pl))
	assert.Equal(t, from.Folder()+"/"+from.Entry(songA.id).File, copied[0].Track.CopiedFrom)
	c := s.collection(singlesSpec())
	assert.Equal(t, library.StatusCopied, c.Entry(songA.id).Status)
	assert.True(t, c.Done(songB.id))
	assert.Contains(t, s.spotify.Commands()[start:], "next", "Spotify moves on from the copied song")
}

func TestPartialTakeNeverReplacesACompleteRecording(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	before := *s.collection(singlesSpec()).Entry(songA.id)

	s.reopen()
	s.resetEvents()
	controls := make(chan Control, 1)
	s.at[s.now().Sub(s.start)+3*time.Second] = func(*sim) { controls <- ControlStop }
	_, res = s.run(singles(songA), Options{Redo: true, KeepPartial: true}, controls)
	require.NoError(t, res.Err)
	assert.Equal(t, OutcomeStopped, res.Outcome)
	assert.Empty(t, s.eventsOf(EventTrackSaved))
	discarded := s.eventsOf(EventTrackDiscarded)
	require.Len(t, discarded, 1)
	assert.Equal(t, library.ErrKeptComplete.Error(), discarded[0].Reason)
	assert.Equal(t, 1, res.Counts.Discarded)
	after := s.collection(singlesSpec()).Entry(songA.id)
	assert.False(t, after.Partial)
	assert.Equal(t, before.AudioSHA256, after.AudioSHA256, "the complete file stays")
}

func TestCopyIntoAFolderThatCannotTakeFilesEndsTheSession(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	s := newSim(t, songA)
	p1 := s.playlist("pl0000000000000000000m", "One", true, songA)
	p2 := s.playlist("pl0000000000000000000o", "Two", true, songA)
	cols := []spotify.Collection{p1, p2}
	require.NoError(t, Prepare(s.lib, BuildPlan(s.lib, cols, PlanOptions{})))
	// The second folder stops taking files once the session is under way:
	// Alpha records into the first, and its copy into the second fails.
	s.readOnlyAt(time.Second, s.collection(playlistSpec(p2)).Dir)

	_, res := s.run(cols, Options{}, nil)
	require.ErrorIs(t, res.Err, library.ErrStore)
	require.ErrorContains(t, res.Err, songA.name+": ")
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, 0, res.Counts.Copied)
	assert.Equal(t, library.StatusRecorded, s.collection(playlistSpec(p1)).Entry(songA.id).Status)
	assert.Equal(t, library.StatusPending, s.collection(playlistSpec(p2)).Entry(songA.id).Status,
		"the copy is left for the next run")
}

func TestCopyOfAnUnreadableRecordingFailsTheSong(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	sc := s.collection(singlesSpec())
	require.NoError(t, os.WriteFile(sc.Path(sc.Entry(songA.id)), []byte("not an m4a"), 0o600))

	s.reopen()
	s.resetEvents()
	start := len(s.spotify.Commands())
	pl := s.playlist("pl0000000000000000000p", "Later", true, songA)
	plan, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	require.NoError(t, res.Err, "one bad source file is not a reason to stop")
	assert.Equal(t, 1, plan.Count(ActionCopy))
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Failed)
	failed := s.eventsOf(EventTrackFailed)
	require.Len(t, failed, 1)
	assert.True(t, strings.HasPrefix(failed[0].Reason, "copy: "), failed[0].Reason)
	assert.Equal(t, library.StatusFailed, s.collection(playlistSpec(pl)).Entry(songA.id).Status)
	assert.Len(t, s.spotify.Commands(), start, "a copy plays nothing")
}

func TestStatusThatCannotBeSavedIsAWarning(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	marathon := simTrack{id: "0000000000000000000lng", name: "Marathon", artist: "Drone", dur: 40 * time.Hour}
	s := newSim(t, marathon)
	cols := singles(marathon)
	require.NoError(t, Prepare(s.lib, BuildPlan(s.lib, cols, PlanOptions{})))
	s.readOnlyAt(stepDur, s.collection(singlesSpec()).Dir)

	_, res := s.run(cols, Options{}, nil)
	require.NoError(t, res.Err, "a status that cannot be written does not end the session")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Failed)
	warnings := messages(s.eventsOf(EventWarning))
	assert.True(t, hasPrefix(warnings, "library: save manifest"), "warnings: %v", warnings)
}

func TestLiveSongTheLibraryCannotListIsStillRecorded(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	s := newSim(t, songA)
	require.NoError(t, Prepare(s.lib, BuildPlan(s.lib, nil, PlanOptions{})))
	s.readOnlyAt(stepDur, s.collection(singlesSpec()).Dir)
	s.listenerPlays(false, songA.id)

	_, res := s.run(nil, Options{}, nil)
	// The new song's pending entry cannot be written: a warning, and the
	// song records on. Its file cannot be stored either, which ends the
	// session.
	warnings := messages(s.eventsOf(EventWarning))
	assert.True(t, hasPrefix(warnings, "library: save manifest"), "warnings: %v", warnings)
	assert.Len(t, s.eventsOf(EventTrackStarted), 1)
	require.ErrorIs(t, res.Err, library.ErrStore)
	assert.Equal(t, 0, res.Counts.Recorded)
}

func TestLiveModeDiscardsAnIncompleteSongWithoutRetryingIt(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.listenerPlays(false, songA.id, songB.id)
	// The listener skips Alpha in the Spotify app.
	s.at[3*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.idx, f.pos = 1, 0 }) }

	_, res := s.run(nil, Options{IdleTimeout: 3 * time.Second}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Discarded)
	discarded := s.eventsOf(EventTrackDiscarded)
	require.Len(t, discarded, 1)
	assert.Equal(t, songA.id, discarded[0].Track.ID)
	assert.Equal(t, ReasonEndedEarly, discarded[0].Reason)
	c := s.collection(singlesSpec())
	assert.Equal(t, library.StatusPending, c.Entry(songA.id).Status)
	assert.True(t, c.Done(songB.id))
	assert.Zero(t, s.spotify.plays(0), "live mode never asks Spotify to play")
	assert.Contains(t, messages(s.eventsOf(EventWarning)), "nothing to record for 3s; ending the session")
}

func TestLiveModeSkipsASongItRecordedWhenItComesAroundAgain(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.listenerPlays(true, songA.id, songB.id)

	_, res := s.run(nil, Options{IdleTimeout: 3 * time.Second}, nil)
	requireComplete(t, res)
	assert.Equal(t, 2, res.Counts.Recorded)
	assert.Len(t, s.eventsOf(EventTrackSaved), 2, "each song is recorded once")
	assert.Contains(t, s.spotify.Commands(), "next", "a song already recorded is skipped")
	assert.True(t, s.spotify.repeat, "live mode leaves repeat alone")
}
