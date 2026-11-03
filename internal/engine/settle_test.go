package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/wav"
)

func TestVerboseReportsTheBoundaryOfEveryTake(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	// The first play resumes another track mid-song: a false start of
	// Alpha, then the take that records it.
	s.spotify.resume = map[string]resumeSpot{songA.id: {track: songB.id, pos: 2500 * time.Millisecond}}

	_, res := s.run(singles(songA), Options{Verbose: true}, nil)
	requireComplete(t, res)
	assert.Empty(t, s.eventsOf(EventTrackDiscarded))
	bounds := s.eventsOf(EventBoundary)
	require.Len(t, bounds, 2, "one for the false start, one for the recorded take")
	for _, b := range bounds {
		require.NotNil(t, b.Track)
		assert.Equal(t, songA.id, b.Track.ID)
		require.NotNil(t, b.DriftSeconds)
	}
	assert.NotEmpty(t, bounds[0].Reason, "the false start was not a complete take")
	take := bounds[1]
	assert.Empty(t, take.Reason)
	assert.Equal(t, 1, take.Runs)
	assert.Less(t, *take.DriftSeconds, DefaultTuning().MaxDrift.Seconds())
	require.NotNil(t, take.OffsetSeconds)
	// The simulated capture has no delay of its own: the song's audio
	// starts the tuned latency after the first observation, less the part
	// of a poll the song had already played by then.
	latency, poll := DefaultTuning().Latency.Seconds(), pollDur.Seconds()
	assert.InDelta(t, latency-poll/2, *take.OffsetSeconds, poll/2+0.005)
}

func TestSongsEndingEarlyByTheSameAmountEndTheSessionAsCrossfade(t *testing.T) {
	t.Parallel()
	const dur = 10 * time.Second
	a, b := longSong("0000000000000000000ea1", dur), longSong("0000000000000000000eb2", dur)
	s := newSim(t, a, b, songC)
	// Both blend into the next song 3 s before their end, without Spotify
	// ever reporting a shorter duration (the check that usually gives
	// crossfade away).
	s.spotify.cutEarly[a.id], s.spotify.cutEarly[b.id] = 3*time.Second, 3*time.Second
	pl := s.playlist("pl0000000000000000000q", "Blended", true, a, b, songC)

	_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
	require.ErrorIs(t, res.Err, ErrCrossfade)
	require.ErrorContains(t, res.Err, "before the end")
	assert.Equal(t, 0, res.Counts.Recorded)
	// Charlie, which was playing when the session ended, may be discarded
	// as cut short too; the two blended songs are discarded as ended early.
	var blended []string
	for _, d := range s.eventsOf(EventTrackDiscarded) {
		if d.Track.ID != songC.id {
			assert.Equal(t, ReasonEndedEarly, d.Reason)
			blended = append(blended, d.Track.ID)
		}
	}
	assert.Equal(t, []string{a.id, b.id}, blended)
	assert.Equal(t, 1, s.spotify.plays(0), "the second song flowed on; nothing was retried")
}

func TestEarlyEndsThatDoNotRepeatAreNotCrossfade(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		dur          time.Duration
		cutA, cutB   time.Duration
		recorded     int
		failed       int
		wantDiscards int
	}{
		"a song played through in between resets the count": {
			dur: 10 * time.Second, cutA: 3 * time.Second, recorded: 2, failed: 1, wantDiscards: 2,
		},
		"they end early by different amounts": {
			dur: 10 * time.Second, cutA: 3 * time.Second, cutB: 6 * time.Second, recorded: 1, failed: 2, wantDiscards: 4,
		},
		"they end further from the end than a crossfade reaches": {
			dur: 20 * time.Second, cutA: 14 * time.Second, cutB: 14 * time.Second, recorded: 1, failed: 2, wantDiscards: 4,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, b := longSong("0000000000000000000ra1", tc.dur), longSong("0000000000000000000rb2", tc.dur)
			s := newSim(t, a, b, songC)
			// Charlie, played through, follows them: a song moving on early
			// always moves on to another.
			s.spotify.cutEarly[a.id], s.spotify.cutEarly[b.id] = tc.cutA, tc.cutB
			pl := s.playlist("pl0000000000000000000r", "Uneven", true, a, b, songC)

			_, res := s.run([]spotify.Collection{pl}, Options{}, nil)
			require.NoError(t, res.Err, "the songs are discarded and retried, not taken for crossfade")
			assert.Equal(t, tc.recorded, res.Counts.Recorded)
			assert.Equal(t, tc.failed, res.Counts.Failed)
			assert.Equal(t, tc.wantDiscards, res.Counts.Discarded)
		})
	}
}

func TestRenderFailureFailsTheSongWithoutPlayingItAgain(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	for name, breakWork := range map[string]func(dir string) error{
		"the work folder is a file": func(dir string) error {
			return os.WriteFile(dir, nil, 0o600)
		},
		"the work folder is read-only": func(dir string) error {
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			return os.Chmod(dir, 0o500)
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB)
			work := filepath.Join(s.home, "work")
			require.NoError(t, breakWork(work))
			t.Cleanup(func() { _ = os.Chmod(work, 0o700) })

			_, res := s.run(singles(songA, songB), Options{MaxFailures: 2}, nil)
			require.ErrorIs(t, res.Err, ErrFailing, "two songs in a row could not be written")
			require.ErrorContains(t, res.Err, "(2 in a row; the last: render: ")
			assert.Equal(t, 0, res.Counts.Recorded)
			assert.Equal(t, 2, res.Counts.Failed)
			failed := s.eventsOf(EventTrackFailed)
			require.Len(t, failed, 2)
			for _, ev := range failed {
				assert.True(t, strings.HasPrefix(ev.Reason, "render: "), ev.Reason)
			}
			assert.Equal(t, 2, s.spotify.plays(0), "a song that cannot be written is not retried")
			assert.Equal(t, library.StatusFailed, s.collection(singlesSpec()).Entry(songA.id).Status)
		})
	}
}

func TestStreakErrorNamesWhatTheSetbacksShare(t *testing.T) {
	t.Parallel()
	const last = "Spotify did not start it"
	for name, tc := range map[string]struct {
		streak []setbackKind
		want   error
		text   string
	}{
		"all silent":    {[]setbackKind{setbackSilent, setbackSilent}, ErrNoSound, " (2 in a row)"},
		"none started":  {[]setbackKind{setbackNotStarted, setbackNotStarted, setbackNotStarted}, ErrNotStarting, " (3 in a row)"},
		"mixed reasons": {[]setbackKind{setbackSilent, setbackNotStarted}, ErrFailing, " (2 in a row; the last: " + last + ")"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{sessionState: sessionState{streak: tc.streak, lastSetback: last}}
			err := e.streakError()
			require.ErrorIs(t, err, tc.want)
			assert.Equal(t, tc.want.Error()+tc.text, err.Error())
		})
	}
}

func TestTooLongIsJudgedAtTheCaptureFormat(t *testing.T) {
	t.Parallel()
	limit := wav.MaxDuration(testRate, 2, 16)
	require.Positive(t, limit)
	e, err := New(Config{Player: &fakeSpotify{}, Library: &library.Library{}, Source: newFakeSource()})
	require.NoError(t, err)
	assert.Empty(t, e.tooLong(limit), "a song that just fits is recorded")
	assert.Empty(t, e.tooLong(0), "an unknown duration is no reason to refuse")
	reason := e.tooLong(limit + time.Hour)
	assert.Contains(t, reason, "too long to record")
	assert.Contains(t, reason, "at 8000 Hz, 16-bit")

	src := newFakeSource()
	src.bits = 24
	deep, err := New(Config{Player: &fakeSpotify{}, Library: &library.Library{}, Source: src})
	require.NoError(t, err)
	limit24 := wav.MaxDuration(testRate, 2, 24)
	assert.Less(t, limit24, limit, "24-bit samples fill the WAV a third faster")
	assert.Empty(t, deep.tooLong(limit24))
	assert.Contains(t, deep.tooLong(limit24+time.Minute), "at 8000 Hz, 24-bit")

	noSource, err := New(Config{Player: &fakeSpotify{}, Library: &library.Library{}})
	require.NoError(t, err)
	assert.Empty(t, noSource.tooLong(limit+time.Hour), "without a capture format there is no limit to apply")
}
