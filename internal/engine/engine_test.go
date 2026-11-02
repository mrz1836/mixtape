package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// Songs used across the scenarios (short, so the simulation stays fast).
//
//nolint:gochecknoglobals // fixed test data
var (
	songA = simTrack{id: "0000000000000000000aaa", name: "Alpha", artist: "Artist One", dur: 6 * time.Second}
	songB = simTrack{id: "0000000000000000000bbb", name: "Bravo", artist: "Artist Two", dur: 5 * time.Second}
	songC = simTrack{id: "0000000000000000000ccc", name: "Charlie", artist: "Artist Three", dur: 7 * time.Second}
	songD = simTrack{id: "0000000000000000000ddd", name: "Delta", artist: "Artist Four", dur: 5 * time.Second}
	adX   = simTrack{id: "adxx", name: "Advertisement", dur: 3 * time.Second, ad: true}
)

// framesOf converts a song duration to test-rate frames.
func framesOf(d time.Duration) int64 { return int64(d.Seconds() * testRate) }

func requireComplete(t *testing.T, res Result) {
	t.Helper()
	require.NoError(t, res.Err)
	require.Equal(t, OutcomeComplete, res.Outcome, "counts: %+v", res.Counts)
}

// collection loads the manifest of a folder by spec.
func (s *sim) collection(spec library.Spec) *library.Collection {
	s.t.Helper()
	c, ok := s.lib.Lookup(spec)
	require.True(s.t, ok, "collection %+v", spec)
	return c
}

func singlesSpec() library.Spec { return library.Spec{Kind: library.KindSingles, Name: SinglesFolder} }

func TestQueueRecordsSinglesAndSkipsThemNextTime(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.shuffle = true

	plan, res := s.run(singles(songA, songB), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, ModeQueue, plan.Mode)
	assert.Equal(t, 2, res.Counts.Recorded)

	c := s.collection(singlesSpec())
	for _, id := range []string{songA.id, songB.id} {
		e := c.Entry(id)
		require.NotNil(t, e)
		assert.Equal(t, library.StatusRecorded, e.Status)
		assert.FileExists(t, c.Path(e))
		assert.NotEmpty(t, e.AudioSHA256)
	}
	// Every cut is the song plus a margin at each edge, within a poll.
	margin := framesOf(DefaultTuning().Margin)
	for _, n := range s.enc.totalFrames() {
		assert.True(t, n == framesOf(songA.dur)+2*margin || n == framesOf(songB.dur)+2*margin ||
			abs64(n-framesOf(songA.dur)-2*margin) < framesOf(pollDur) ||
			abs64(n-framesOf(songB.dur)-2*margin) < framesOf(pollDur), "frames %d", n)
	}
	assert.Equal(t, []string{EventTrackStarted, EventTrackSaved, EventTrackStarted, EventTrackSaved},
		filter(s.types(), EventTrackStarted, EventTrackSaved, EventSessionFinished))
	assert.True(t, s.spotify.shuffle, "shuffle is restored after the session")
	assert.Contains(t, s.spotify.Commands(), "pause", "Spotify is paused when the queue is done")

	// A second run finds both songs done: nothing plays.
	s.reopen()
	s.resetEvents()
	before := len(s.spotify.Commands())
	plan, res = s.run(singles(songA, songB), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 2, plan.Count(ActionSkip))
	assert.Equal(t, 2, res.Counts.Skipped)
	assert.Len(t, s.spotify.Commands(), before, "no playback for songs already recorded")
}

func TestPlaylistFlowsInContextAndCopiesKnownSongs(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	_, res := s.run(singles(songB), Options{}, nil)
	requireComplete(t, res)

	s.reopen()
	s.resetEvents()
	start := len(s.spotify.Commands())
	pl := s.playlist("pl0000000000000000000a", "Road Trip", true, songA, songB, songC)
	plan, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	requireComplete(t, res)

	assert.Equal(t, 1, plan.Count(ActionCopy), "Bravo is already in Singles")
	assert.Equal(t, 2, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Copied)
	c := s.collection(library.Spec{Kind: library.KindPlaylist, SpotifyID: pl.Ref.ID})
	assert.Equal(t, library.StatusCopied, c.Entry(songB.id).Status)
	assert.FileExists(t, filepath.Join(c.Dir, c.Folder()+".m3u8"))

	plays := 0
	for _, cmd := range s.spotify.Commands()[start:] {
		if len(cmd) > 5 && cmd[:5] == "play " {
			plays++
		}
	}
	// Alpha starts the context; after it Spotify plays Bravo (copied), so
	// mixtape moves to Charlie: two requests, no more.
	assert.Equal(t, 2, plays, "commands: %v", s.spotify.Commands()[start:])
}

func TestHundredsOfCopiesAtTheStartDoNotHang(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)

	// Alpha is in 135 playlists: 135 copies are queued before the session
	// loop runs, far more than the worker's result buffer holds.
	s.reopen()
	s.resetEvents()
	cols := make([]spotify.Collection, 0, 135)
	for i := range 135 {
		cols = append(cols, s.playlist(fmt.Sprintf("pl%020d", i), fmt.Sprintf("List %d", i), true, songA))
	}
	done := make(chan Result, 1)
	go func() {
		_, r := s.run(cols, Options{}, nil)
		done <- r
	}()
	select {
	case res = <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("the session hung queueing copies")
	}
	requireComplete(t, res)
	assert.Equal(t, 135, res.Counts.Copied)
}

func TestOutputRateChangeReportedAsDeviceChangeKeepsRecording(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	// Headphones at another rate plugged in during Alpha: the tap rebuilds
	// on the new device and reports a device change.
	s.at[2*time.Second] = func(s *sim) {
		s.source.mu.Lock()
		s.source.rate = 2 * testRate
		s.source.mu.Unlock()
		s.source.events <- capture.Event{Kind: capture.EventDeviceChanged, Detail: "the default output device changed"}
	}
	pl := s.playlist("pl0000000000000000000r", "Rates", true, songA, songB, songC)

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 3, res.Counts.Recorded, "songs after the change are captured at the new rate")
}

func TestUnwritableLibraryEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	pl := s.playlist("pl0000000000000000000w", "Gone", true, songA, songB)
	// The playlist's folder (created before the session starts) turns
	// read-only, as an unplugged drive would refuse writes.
	var dirs []string
	s.at[time.Second] = func(s *sim) {
		entries, err := os.ReadDir(s.root)
		require.NoError(t, err)
		for _, e := range entries {
			if e.IsDir() {
				dir := filepath.Join(s.root, e.Name())
				require.NoError(t, os.Chmod(dir, 0o500))
				dirs = append(dirs, dir)
			}
		}
	}
	t.Cleanup(func() {
		for _, d := range dirs {
			_ = os.Chmod(d, 0o700)
		}
	})

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	require.ErrorIs(t, res.Err, library.ErrStore)
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Equal(t, 2, res.Counts.Remaining, "both songs stay pending for the next run")
	wavs, _ := filepath.Glob(filepath.Join(s.home, "work", "*.wav"))
	assert.Empty(t, wavs, "no WAV is leaked")
}

var errDiskFull = errors.New("no space left on device")

func TestSpoolWriteErrorEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.at[2*time.Second] = func(s *sim) {
		s.eng.spoolMu.Lock()
		s.eng.spoolErr = fmt.Errorf("write spool: %w", errDiskFull)
		s.eng.spoolMu.Unlock()
	}
	_, res := s.run(singles(songA, songB), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrSpoolWrite)
	assert.Contains(t, res.Err.Error(), "no space left on device")
	assert.Equal(t, 0, res.Counts.Recorded)
}

func TestSilentSongsInARowEndTheSession(t *testing.T) {
	t.Parallel()
	long := func(id string) simTrack {
		return simTrack{id: id, name: "Song " + id[len(id)-3:], artist: "X", dur: 14 * time.Second}
	}
	b, c, d := long("0000000000000000000sb1"), long("0000000000000000000sc2"), long("0000000000000000000sd3")
	s := newSim(t, songA, b, c, d)
	// Alpha is heard; then the sound goes elsewhere (Spotify Connect, a
	// muted app) while Spotify keeps playing.
	s.at[7*time.Second] = func(s *sim) {
		s.source.mu.Lock()
		s.source.silent = true
		s.source.mu.Unlock()
	}
	pl := s.playlist("pl0000000000000000000s", "Quiet", true, songA, b, c, d)

	_, res := s.run([]spotify.Collection{pl}, Options{MaxFailures: 3}, nil)
	require.ErrorIs(t, res.Err, ErrNoSound)
	assert.Contains(t, res.Err.Error(), "3 in a row")
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Positive(t, res.Counts.Remaining, "the session stopped instead of trying every song")
}

func TestSongsThatNeverStartEndTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	s.spotify.ignorePlay = true // signed out: "play track" does nothing
	pl := s.playlist("pl0000000000000000000n", "Nothing", true, songA, songB, songC)

	_, res := s.run([]spotify.Collection{pl}, Options{MaxFailures: 3, StartTimeout: time.Second}, nil)
	require.ErrorIs(t, res.Err, ErrNotStarting)
	assert.Equal(t, 0, res.Counts.Recorded)
}

func TestMaxFailuresZeroNeverStops(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.ignorePlay = true
	pl := s.playlist("pl0000000000000000000z", "Patient", true, songA, songB)

	_, res := s.run([]spotify.Collection{pl}, Options{StartTimeout: time.Second}, nil)
	require.NoError(t, res.Err, "without a limit every song is tried")
	assert.Equal(t, 2, res.Counts.Failed)
}

func TestSpotifyRelaunchingMidSessionIsNotFatal(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	// Spotify quits and comes back: for a moment its process is listed but
	// does not answer, which reads as blocked Apple Events.
	s.at[7*time.Second] = func(s *sim) {
		s.spotify.user(func(f *fakeSpotify) {
			f.failNext, f.failErr = 4, fmt.Errorf("%w: Connection is invalid (-609)", spotify.ErrAppleEventsBlocked)
		})
	}
	_, res := s.run(singles(songA, songB), Options{}, nil)
	require.NoError(t, res.Err)
	assert.Equal(t, 2, res.Counts.Recorded)
}

func TestBlockedAppleEventsBeforeSpotifyAnswersAreFatal(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.failNext, s.spotify.failErr = 1000, fmt.Errorf("%w: sandboxed (-600)", spotify.ErrAppleEventsBlocked)
	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorIs(t, res.Err, spotify.ErrAppleEventsBlocked)
}

func TestInterruptedSessionsSpotifyChangesArePutBackAtTheEnd(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	// A crashed --mute session left the volume at 100; it could not be
	// restored before this session started.
	s.spotify.volume = 100
	s.pending = &session.SpotifyState{
		Original: session.PlayerSettings{Volume: ptr(47)},
		Set:      session.PlayerSettings{Volume: ptr(100)},
	}
	_, res := s.run(singles(songA), Options{Mute: true}, nil)
	requireComplete(t, res)
	s.spotify.mu.Lock()
	defer s.spotify.mu.Unlock()
	assert.GreaterOrEqual(t, s.spotify.volume, 47, "the pre-crash volume is back")
	assert.Less(t, s.spotify.volume, 100)
}

func TestMergePending(t *testing.T) {
	t.Parallel()
	crashed := &session.SpotifyState{
		Original: session.PlayerSettings{Shuffle: ptr(true), Volume: ptr(47)},
		Set:      session.PlayerSettings{Shuffle: ptr(false), Volume: ptr(100)},
	}
	// Spotify still as the crash left it: both settings are carried forward.
	got := mergePending(session.SpotifyState{}, crashed, spotify.Snapshot{Shuffle: false, Volume: 100})
	assert.Equal(t, crashed.Original, got.Original)
	assert.Equal(t, crashed.Set, got.Set)

	// The listener changed the volume since: their value stays.
	got = mergePending(session.SpotifyState{}, crashed, spotify.Snapshot{Shuffle: false, Volume: 60})
	assert.Nil(t, got.Original.Volume)
	assert.Equal(t, ptr(true), got.Original.Shuffle)

	// Already restored, or nothing pending: unchanged.
	done := *crashed
	done.Restored = true
	assert.Equal(t, session.SpotifyState{}, mergePending(session.SpotifyState{}, &done, spotify.Snapshot{Volume: 100}))
	assert.Equal(t, session.SpotifyState{}, mergePending(session.SpotifyState{}, nil, spotify.Snapshot{}))
}

func TestUserPauseIsExcised(t *testing.T) {
	t.Parallel()
	s := newSim(t, songC)
	s.at[2*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = false }) }
	s.at[5*time.Second] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = true }) }

	_, res := s.run(singles(songC), Options{}, nil)
	requireComplete(t, res)
	frames := s.enc.totalFrames()
	require.Len(t, frames, 1)
	want := framesOf(songC.dur) + 2*framesOf(DefaultTuning().Margin)
	assert.InDelta(t, want, frames[0], float64(framesOf(pollDur)), "the 3 s pause is cut out")
}

func TestSeekDiscardsAndRetriesOnce(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.at[3*time.Second] = func(s *sim) {
		s.spotify.user(func(f *fakeSpotify) { f.pos += 2 * time.Second })
	}
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)

	discarded := s.eventsOf(EventTrackDiscarded)
	require.Len(t, discarded, 1)
	assert.Equal(t, ReasonSeekForward, discarded[0].Reason)
	assert.Equal(t, 1, res.Counts.Recorded, "the retry from the top succeeds")
	assert.Equal(t, 1, res.Counts.Discarded)
}

func TestUnplayableSongIsMarkedAndTheNextRecorded(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.unplayable[songA.id] = true
	pl := s.playlist("pl0000000000000000000b", "Gaps", true, songA, songB)

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	require.NoError(t, res.Err)
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Failed)
	c := s.collection(library.Spec{Kind: library.KindPlaylist, SpotifyID: pl.Ref.ID})
	assert.Equal(t, library.StatusUnavailable, c.Entry(songA.id).Status)
	assert.Equal(t, library.StatusRecorded, c.Entry(songB.id).Status)
}

func TestRelinkedSongIsRecordedUnderTheRequestedID(t *testing.T) {
	t.Parallel()
	relinked := songA
	relinked.id = "0000000000000000000zzz"
	s := newSim(t, songA, relinked)
	s.spotify.relink[songA.id] = relinked.id

	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	e := s.collection(singlesSpec()).Entry(songA.id)
	require.NotNil(t, e)
	assert.Equal(t, library.StatusRecorded, e.Status)
	assert.Equal(t, relinked.id, e.PlayedID)
}

func TestAdsAreNotRecorded(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, adX, songB)
	pl := s.playlist("pl0000000000000000000c", "With Ads", true, songA, songB)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, adX.id, songB.id}

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 2, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded))
}

func TestStopDiscardsThePartialSong(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	controls := make(chan Control, 1)
	s.at[2*time.Second] = func(*sim) { controls <- ControlStop }

	_, res := s.run(singles(songA, songB), Options{}, controls)
	require.NoError(t, res.Err)
	assert.Equal(t, OutcomeStopped, res.Outcome)
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Equal(t, 2, res.Counts.Remaining)
	e := s.collection(singlesSpec()).Entry(songA.id)
	assert.Equal(t, library.StatusPending, e.Status, "a partial never counts as done")
	assert.Contains(t, s.spotify.Commands(), "pause")
}

func TestKeepPartialSavesAFlaggedFile(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	controls := make(chan Control, 1)
	s.at[3*time.Second] = func(*sim) { controls <- ControlStop }

	_, res := s.run(singles(songA), Options{KeepPartial: true}, controls)
	require.NoError(t, res.Err)
	saved := s.eventsOf(EventTrackSaved)
	require.Len(t, saved, 1)
	assert.True(t, saved[0].Track.Partial)
	e := s.collection(singlesSpec()).Entry(songA.id)
	assert.True(t, e.Partial)
	assert.False(t, s.collection(singlesSpec()).Done(songA.id))
}

func TestSilentCaptureEndsTheSession(t *testing.T) {
	t.Parallel()
	long := simTrack{id: "0000000000000000000lll", name: "Long", artist: "X", dur: 30 * time.Second}
	s := newSim(t, long)
	s.source.silent = true
	_, res := s.run(singles(long), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrSilentDevice)
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
}

func TestJoinedMidSongRestartsFromTheTop(t *testing.T) {
	t.Parallel()
	s := newSim(t, songC)
	s.spotify.queue, s.spotify.idx, s.spotify.pos, s.spotify.playing = []string{songC.id}, 0, 3*time.Second, true

	_, res := s.run(singles(songC), Options{}, nil)
	requireComplete(t, res)
	assert.Contains(t, s.spotify.Commands(), "seek")
	assert.Equal(t, 1, res.Counts.Recorded)
}

func TestLiveModeSkipsDoneSongsAndRecordsNewOnes(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)

	s.reopen()
	s.resetEvents()
	s.spotify.user(func(f *fakeSpotify) {
		f.queue, f.idx, f.pos, f.playing = []string{songA.id, songB.id}, 0, 0, true
	})
	_, res = s.run(nil, Options{Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Skipped, "Alpha is done: skipped with next")
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Contains(t, s.spotify.Commands(), "next")
	assert.NotContains(t, s.spotify.Commands()[len(s.spotify.Commands())-1], "pause", "live mode leaves Spotify alone")
	assert.True(t, s.collection(singlesSpec()).Done(songB.id))
}

func TestLiveModeMatchFilter(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.user(func(f *fakeSpotify) { f.queue, f.playing = []string{songA.id, songB.id}, true })
	_, res := s.run(nil, Options{Match: "brav", Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.True(t, s.collection(singlesSpec()).Done(songB.id))
	assert.Nil(t, s.collection(singlesSpec()).Entry(songA.id), "Alpha was left alone")
}

func TestFollowModeRecordsPastTheKnownListUntilTheLoop(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC, songD)
	// The embed listed only the first two of four songs.
	pl := s.playlist("pl0000000000000000000d", "Huge", false, songA, songB)
	s.spotify.contexts[pl.Ref.URI()] = []string{songA.id, songB.id, songC.id, songD.id}

	plan, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, ModeFollow, plan.Mode)
	assert.Equal(t, 4, res.Counts.Recorded)
	c := s.collection(library.Spec{Kind: library.KindPlaylist, SpotifyID: pl.Ref.ID})
	for _, id := range []string{songA.id, songB.id, songC.id, songD.id} {
		assert.True(t, c.Done(id), id)
	}
	assert.False(t, s.spotify.repeat, "repeat is restored")
}

func TestFollowModeDetectsTheLoopWhenTheFirstSongIsUnplayable(t *testing.T) {
	t.Parallel()
	x := simTrack{id: "0000000000000000000xxx", name: "Region Locked", artist: "Nobody", dur: 5 * time.Second}
	s := newSim(t, x, songA, songB, songC, songD)
	s.spotify.unplayable[x.id] = true // Spotify skips it on every pass
	// The embed listed the first three of five songs.
	pl := s.playlist("pl0000000000000000000u", "Huge", false, x, songA, songB)
	s.spotify.contexts[pl.Ref.URI()] = []string{x.id, songA.id, songB.id, songC.id, songD.id}

	plan, res := s.run([]spotify.Collection{pl}, Options{IdleTimeout: time.Hour}, nil)
	require.NoError(t, res.Err)
	assert.Equal(t, ModeFollow, plan.Mode)
	assert.Equal(t, 4, res.Counts.Recorded)
	assert.Less(t, s.now().Sub(s.start), 2*time.Minute, "the loop ends the session, not the idle timeout")
}

func TestDuplicateAcrossPlaylistsIsRecordedOnceAndCopied(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	p1 := s.playlist("pl0000000000000000000e", "One", true, songA)
	p2 := s.playlist("pl0000000000000000000f", "Two", true, songB, songA)

	plan, res := s.run([]spotify.Collection{p1, p2}, Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, plan.Count(ActionCopy))
	assert.Equal(t, 2, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Copied)
	c2 := s.collection(library.Spec{Kind: library.KindPlaylist, SpotifyID: p2.Ref.ID})
	assert.Equal(t, library.StatusCopied, c2.Entry(songA.id).Status)
}

func TestEncodeFailureKeepsTheWAVAndRetriesNextRun(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.enc.fail = true
	_, res := s.run(singles(songA), Options{}, nil)
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	e := s.collection(singlesSpec()).Entry(songA.id)
	assert.Equal(t, library.StatusEncodeFailed, e.Status)
	_, err := os.Stat(e.PendingWAV)
	require.NoError(t, err)

	s.enc.fail = false
	s.reopen()
	plan, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, plan.Count(ActionEncode))
	assert.True(t, s.collection(singlesSpec()).Done(songA.id))
}

func TestDryRunPlanChangesNothing(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	plan := BuildPlan(s.lib, singles(songA), PlanOptions{})
	assert.Equal(t, []PlanItem{{
		Action: "record", ID: songA.id, Title: "Alpha", Artist: "Artist One",
		Collection: SinglesFolder, Position: 1, DurationMS: 6000,
	}}, plan.Summary())
	entries, err := os.ReadDir(s.root)
	require.NoError(t, err)
	assert.Empty(t, entries, "planning creates no folders")
}

func TestCrossfadeEndsTheSession(t *testing.T) {
	t.Parallel()
	long := func(id string) simTrack {
		return simTrack{id: id, name: "Song " + id[len(id)-3:], artist: "X", dur: 20 * time.Second}
	}
	a, b, c, d := long("0000000000000000000xa1"), long("0000000000000000000xb2"), long("0000000000000000000xc3"), long("0000000000000000000xd4")
	s := newSim(t, a, b, c, d)
	s.spotify.crossfade = 6 * time.Second
	pl := s.playlist("pl0000000000000000000x", "Faded", true, a, b, c, d)

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	require.ErrorIs(t, res.Err, ErrCrossfade, "the first song's duration shrinking gives it away")
	assert.Contains(t, res.Err.Error(), "by 6.0s")
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Discarded)
}

func TestCrossfadeAllowedRecordsToTheShortenedEnd(t *testing.T) {
	t.Parallel()
	a := simTrack{id: "0000000000000000000xe5", name: "Faded", artist: "X", dur: 12 * time.Second}
	s := newSim(t, a)
	s.spotify.crossfade = 4 * time.Second
	_, res := s.run(singles(a), Options{AllowCrossfade: true}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
}

func TestTransitionalSnapshotIsIgnored(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	e, err := New(Config{Player: s.spotify, Library: s.lib, Now: s.now})
	require.NoError(t, err)
	e.plan = &Plan{Mode: ModeLive}
	e.cur = &take{key: spotify.TrackURI(songA.id), track: spotify.Track{Name: "Money", Duration: 380 * time.Second}}
	glitch := spotify.Snapshot{State: spotify.StatePlaying, Track: spotify.Track{URI: spotify.TrackURI(songA.id), Name: "Us and Them"}}
	assert.True(t, e.transitional(glitch))
	glitch.Track.Name = "Money"
	assert.False(t, e.transitional(glitch))
}

func TestFalseStartIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.startGlitch = true // every "play track" restarts the song 0.2 s in

	_, res := s.run(singles(songA, songB), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 2, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded), "a restart at the top is not a discarded take")
}

func TestLimitCountsSongsInFlight(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB, songC)
	pl := s.playlist("pl0000000000000000000g", "Three", true, songA, songB, songC)
	_, res := s.run([]spotify.Collection{pl}, Options{Limit: 2}, nil)
	requireComplete(t, res)
	assert.Equal(t, 2, res.Counts.Recorded, "the third song flows in while the second is saved, but is not recorded")
	assert.Len(t, s.eventsOf(EventTrackStarted), 2)
}

func TestStalePositionAfterPlayIsAFalseStart(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.stalePlay = true // the old track's 12 s position shows under the new song first

	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded), "a stale position then 0 is not a failed take")
}

// Live, Spotify either holds the song at exactly 0:00 until it resumes, or
// first plays a fraction of a second from the top (moving).
func resumeStarts() map[string]bool {
	return map[string]bool{"held at 0:00": false, "moving from 0:00": true}
}

func TestResumedSongIsStartedOverFromTheTop(t *testing.T) {
	t.Parallel()
	for name, moving := range resumeStarts() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA)
			// Without a context, "play track" picks Alpha up where it was last left.
			s.spotify.resume = map[string]resumeSpot{songA.id: {pos: 3 * time.Second, moving: moving}}

			_, res := s.run(singles(songA), Options{}, nil)
			requireComplete(t, res)
			assert.Equal(t, 1, res.Counts.Recorded)
			assert.Empty(t, s.eventsOf(EventTrackDiscarded), "a resume is not a failed take")
			assert.Len(t, s.eventsOf(EventTrackStarted), 1, "the abandoned take was never announced")
			assert.Contains(t, s.spotify.Commands(), "seek", "the song is started over")
			saved := s.eventsOf(EventTrackSaved)
			require.Len(t, saved, 1)
			assert.Equal(t, 1, saved[0].Track.Attempt, "the resume did not count as an attempt")
		})
	}
}

func TestSpotifyResumingAnotherTrackIsAFalseStart(t *testing.T) {
	t.Parallel()
	for name, moving := range resumeStarts() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB)
			// As seen live: the first play resumes another track, mid-song,
			// where Spotify last left it; it plays on until mixtape asks again.
			s.spotify.resume = map[string]resumeSpot{songA.id: {track: songB.id, pos: 2500 * time.Millisecond, moving: moving}}

			_, res := s.run(singles(songA), Options{}, nil)
			requireComplete(t, res)
			assert.Equal(t, 1, res.Counts.Recorded)
			assert.Empty(t, s.eventsOf(EventTrackDiscarded), "a false start is not a failed take")
			assert.Len(t, s.eventsOf(EventTrackStarted), 1, "a false start leaves no trace")
			saved := s.eventsOf(EventTrackSaved)
			require.Len(t, saved, 1)
			assert.Equal(t, 1, saved[0].Track.Attempt)
		})
	}
}

func TestNextTrackBlipIsIgnored(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.spotify.blipOnPlay = true // one observation reports the next album track while Alpha loads
	pl := s.playlist("pl0000000000000000000h", "Blip", true, songA, songB)

	_, res := s.run([]spotify.Collection{pl}, Options{Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded), "a one-poll blip is not a track change")
	plays := 0
	for _, c := range s.spotify.Commands() {
		if strings.HasPrefix(c, "play ") {
			plays++
		}
	}
	assert.Equal(t, 1, plays, "the song is requested once")
}

func TestPreviousTrackBlipBeforeTheSongStartsIsIgnored(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	// As seen live: three observations of another track at an old position
	// while the requested song is still at 0:00.
	s.spotify.blipOnPlay, s.spotify.blipLen, s.spotify.blipPos = true, 3, 23*time.Second
	s.spotify.startGlitch = true
	pl := s.playlist("pl0000000000000000000i", "Blip3", true, songA, songB)

	_, res := s.run([]spotify.Collection{pl}, Options{Limit: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded))
}

func TestRelinkMatching(t *testing.T) {
	t.Parallel()
	want := library.Track{Title: "Time - 2011 Remaster", Artist: "Pink Floyd", DurationMS: 413000}
	assert.True(t, relinked(want, spotify.Track{Name: "Time - 2011 remaster", Artist: "Pink Floyd", Duration: 412 * time.Second}))
	assert.False(t, relinked(want, spotify.Track{Name: "Time", Artist: "Pink Floyd", Duration: 413 * time.Second}))
	assert.False(t, relinked(want, spotify.Track{Name: "Time - 2011 Remaster", Artist: "Pink Floyd", Duration: 420 * time.Second}))
	assert.True(t, relinked(library.Track{Title: "X", Artist: "A, B"}, spotify.Track{Name: "x", Artist: "A"}))
}

func filter(types []string, keep ...string) []string {
	var out []string
	for _, t := range types {
		for _, k := range keep {
			if t == k {
				out = append(out, t)
			}
		}
	}
	return out
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
