package engine

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var (
	errNoDevice   = errors.New("the capture device is gone")
	errRefused    = errors.New("spotify refused the command")
	errStateWrite = errors.New("session.json is read-only")
)

// messages returns the messages of events, in order.
func messages(evs []Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		out = append(out, ev.Message)
	}
	return out
}

func TestForcedQuitSettlesTheSongAtOnceAndRestoresSpotify(t *testing.T) {
	t.Parallel()
	for name, keep := range map[string]bool{"the cut song is discarded": false, "--keep-partial keeps what was captured": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB)
			s.spotify.shuffle = true
			// A second Ctrl-C mid-song cancels the context Run was given.
			// Simulated time stops with it, so Alpha's trailing audio never
			// arrives: only settling it at once lets the session end.
			s.at[3*time.Second] = func(s *sim) { s.forceQuit() }

			_, res := s.run(singles(songA, songB), Options{KeepPartial: keep}, nil)
			require.NoError(t, res.Err)
			assert.Equal(t, OutcomeStopped, res.Outcome)
			assert.Equal(t, 0, res.Counts.Recorded)
			assert.Equal(t, 2, res.Counts.Remaining, "both songs stay pending for the next run")
			c := s.collection(singlesSpec())
			e := c.Entry(songA.id)
			require.NotNil(t, e)
			if keep {
				saved := s.eventsOf(EventTrackSaved)
				require.Len(t, saved, 1)
				assert.True(t, saved[0].Track.Partial)
				assert.True(t, e.Partial)
				assert.FileExists(t, c.Path(e))
				assert.Empty(t, s.eventsOf(EventTrackDiscarded))
			} else {
				discarded := s.eventsOf(EventTrackDiscarded)
				require.Len(t, discarded, 1)
				assert.Equal(t, songA.id, discarded[0].Track.ID)
				assert.Equal(t, library.StatusPending, e.Status)
				assert.Empty(t, s.eventsOf(EventTrackSaved))
			}
			assert.True(t, s.spotify.shuffle, "shuffle is put back even after a forced quit")
			assert.Contains(t, s.spotify.Commands(), "pause")
		})
	}
}

func TestProgressHandsOutACopyOfTheTrack(t *testing.T) {
	t.Parallel()
	e, err := New(Config{Player: &fakeSpotify{}, Library: &library.Library{}})
	require.NoError(t, err)
	assert.Nil(t, e.Progress().Track, "no song yet")

	e.progress = Progress{Phase: session.PhaseRecording, Track: &EventTrack{ID: songA.id, Title: songA.name}}
	p := e.Progress()
	require.NotNil(t, p.Track)
	// A status line that edits what it was handed must not reach into the
	// engine, which refreshes the snapshot from another goroutine.
	p.Track.Title = "edited by a caller"
	assert.Equal(t, songA.name, e.Progress().Track.Title)
}

func TestProgressReportsThePhaseTheSongAndTheTimeLeft(t *testing.T) {
	t.Parallel()
	for name, limit := range map[string]int{"without a limit": 0, "with --limit 2": 2} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB, songC)
			var playing, paused Progress
			s.at[2*time.Second] = func(s *sim) { playing = s.eng.Progress() }
			s.at[2500*time.Millisecond] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = false }) }
			s.at[3200*time.Millisecond] = func(s *sim) { paused = s.eng.Progress() }
			s.at[3500*time.Millisecond] = func(s *sim) { s.spotify.user(func(f *fakeSpotify) { f.playing = true }) }

			_, res := s.run(singles(songA, songB, songC), Options{Limit: limit}, nil)
			requireComplete(t, res)

			assert.Equal(t, ModeQueue, playing.Mode)
			assert.Equal(t, session.PhaseRecording, playing.Phase)
			assert.Equal(t, spotify.StatePlaying, playing.State)
			assert.True(t, playing.Recording)
			require.NotNil(t, playing.Track)
			assert.Equal(t, songA.id, playing.Track.ID)
			assert.InDelta(t, songA.dur.Seconds(), playing.Duration, 0.001)
			assert.Positive(t, playing.Position)
			assert.Equal(t, 3, playing.Counts.Remaining)
			// The time left is the rest of this song plus every song still to
			// start; under --limit, only as many as the limit still allows.
			later := songB.dur + songC.dur
			if limit > 0 {
				later = songB.dur
			}
			rest := time.Duration((playing.Duration - playing.Position) * float64(time.Second))
			assert.InDelta(t, (later + rest).Seconds(), playing.ETA.Seconds(), 0.001)

			assert.Equal(t, session.PhasePaused, paused.Phase, "the listener paused Alpha")
			assert.Equal(t, spotify.StatePaused, paused.State)

			assert.Equal(t, session.PhaseStopping, s.eng.Progress().Phase, "a finished session reads as stopping")
		})
	}
}

func TestNewRequiresAPlayerAndALibrary(t *testing.T) {
	t.Parallel()
	_, err := New(Config{Library: &library.Library{}})
	require.ErrorIs(t, err, errConfig)
	_, err = New(Config{Player: &fakeSpotify{}})
	require.ErrorIs(t, err, errConfig)
}

func TestCaptureThatCannotStartEndsTheSessionBeforeAnythingPlays(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		setup   func(t *testing.T, s *sim)
		wantErr string
		wantIs  error
	}{
		"the spool folder cannot be created": {
			setup: func(t *testing.T, s *sim) {
				t.Helper()
				require.NoError(t, os.WriteFile(filepath.Join(s.home, "spool"), nil, 0o600))
			},
			wantErr: "open spool",
		},
		"the source refuses to start": {
			setup:   func(_ *testing.T, s *sim) { s.source.startErr = errNoDevice },
			wantErr: "start capture (fake)",
			wantIs:  errNoDevice,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA)
			s.spotify.shuffle = true
			tc.setup(t, s)

			_, res := s.run(singles(songA), Options{}, nil)
			require.ErrorContains(t, res.Err, tc.wantErr)
			if tc.wantIs != nil {
				require.ErrorIs(t, res.Err, tc.wantIs)
			}
			assert.Equal(t, OutcomeIncomplete, res.Outcome)
			assert.Equal(t, 1, res.Counts.Remaining)
			assert.Zero(t, s.spotify.plays(0), "nothing is played")
			assert.True(t, s.spotify.shuffle, "the shuffle the session turned off is put back")
		})
	}
}

func TestSessionWithoutACaptureSourceEndsBeforeAnythingPlays(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.shuffle = true
	plan := BuildPlan(s.lib, singles(songA), PlanOptions{})
	require.NoError(t, Prepare(s.lib, plan))
	eng, err := New(Config{Player: s.spotify, Library: s.lib, Now: s.now})
	require.NoError(t, err)

	res := eng.Run(t.Context(), plan, nil)
	require.ErrorIs(t, res.Err, errConfig)
	require.ErrorContains(t, res.Err, "no capture source")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Remaining)
	assert.Zero(t, s.spotify.plays(0))
	assert.True(t, s.spotify.shuffle, "the shuffle the session turned off is put back")
}

func TestSpotifySettingsAreSavedBeforeTheyChangeAndAgainOnceRestored(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.shuffle = true // and the volume at 50
	var saved []session.SpotifyState
	s.save = func(st session.SpotifyState) error {
		saved = append(saved, st)
		return nil
	}

	_, res := s.run(singles(songA), Options{Mute: true}, nil)
	requireComplete(t, res)
	require.Len(t, saved, 2, "saved before anything changes, and once restored")
	first := saved[0]
	assert.False(t, first.Restored)
	assert.Equal(t, ptr(true), first.Original.Shuffle)
	assert.Equal(t, ptr(false), first.Set.Shuffle)
	assert.Equal(t, ptr(50), first.Original.Volume)
	assert.Equal(t, ptr(100), first.Set.Volume, "a muted tap records at full volume")
	assert.Nil(t, first.Set.Repeat, "repeat is only for following a playlist")
	assert.True(t, saved[1].Restored)
	assert.Contains(t, s.spotify.Commands(), "volume")
	assert.Equal(t, 50, s.spotify.volume)
	assert.True(t, s.spotify.shuffle)
}

func TestSettingsThatCannotBeSavedAreNotChanged(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.shuffle = true
	s.save = func(session.SpotifyState) error { return errStateWrite }

	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorIs(t, res.Err, errStateWrite)
	require.ErrorContains(t, res.Err, "save spotify settings")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	// A setting changed without a record of the original could not be put
	// back after a crash: nothing is changed, nothing is captured or played.
	assert.Empty(t, s.spotify.Commands())
	assert.True(t, s.spotify.shuffle)
	assert.False(t, s.source.started())
}

func TestIdleTimeoutEndsTheSession(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		cols []spotify.Collection
		opts Options
		want string
	}{
		"live mode with nothing playing ends complete": {
			opts: Options{IdleTimeout: 3 * time.Second},
			want: OutcomeComplete,
		},
		"a queue whose song never starts ends incomplete": {
			cols: singles(songA),
			opts: Options{IdleTimeout: 3 * time.Second, StartTimeout: time.Minute},
			want: OutcomeIncomplete,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA)
			s.spotify.ignorePlay = true

			_, res := s.run(tc.cols, tc.opts, nil)
			require.NoError(t, res.Err)
			assert.Equal(t, tc.want, res.Outcome)
			warnings := s.eventsOf(EventWarning)
			require.Len(t, warnings, 1)
			assert.Equal(t, "nothing to record for 3s; ending the session", warnings[0].Message)
			// The warning is emitted as the session ends: 3 s after the last
			// activity (the start, or the song's request a moment later), as
			// seen by the half-second housekeeping.
			idle := warnings[0].Time.Sub(s.start)
			assert.GreaterOrEqual(t, idle, 3*time.Second)
			assert.Less(t, idle, 4500*time.Millisecond)
		})
	}
}

func TestLimitReachedAfterAFailedSongIsIncomplete(t *testing.T) {
	t.Parallel()
	marathon := simTrack{id: "0000000000000000000lng", name: "Marathon", artist: "Drone", dur: 40 * time.Hour}
	s := newSim(t, marathon, songA, songB)

	_, res := s.run(singles(marathon, songA, songB), Options{Limit: 1}, nil)
	require.NoError(t, res.Err)
	assert.Equal(t, OutcomeIncomplete, res.Outcome, "the limit was reached, but a song failed on the way")
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Failed)
	assert.Equal(t, 1, res.Counts.Remaining, "Bravo is left for the next run")
	assert.Equal(t, 1, s.spotify.plays(0), "only Alpha is played")
}

func TestClosedControlAndCaptureEventChannelsDoNotEndTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	controls := make(chan Control)
	close(controls) // the terminal went away: no more keys, but no stop either
	s.at[time.Second] = func(s *sim) { close(s.source.events) }

	_, res := s.run(singles(songA, songB), Options{}, controls)
	requireComplete(t, res)
	assert.Equal(t, 2, res.Counts.Recorded)
}

func TestSpotifyWatcherEndingEndsTheSession(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.at[2*time.Second] = func(s *sim) { s.spotify.hangUp() }

	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorIs(t, res.Err, ErrSpotifyGone)
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 0, res.Counts.Recorded)
	assert.Equal(t, 1, res.Counts.Remaining)
	assert.Len(t, s.eventsOf(EventTrackDiscarded), 1, "the song cut short is settled")
	assert.Contains(t, s.spotify.Commands(), "pause")
}

func TestLostAutomationPermissionMidSessionIsFatal(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.shuffle = true
	s.at[2*time.Second] = func(s *sim) {
		s.spotify.user(func(f *fakeSpotify) { f.failNext, f.failErr = 1<<20, spotify.ErrNotAuthorized })
	}

	_, res := s.run(singles(songA), Options{}, nil)
	require.ErrorIs(t, res.Err, spotify.ErrNotAuthorized)
	require.NotErrorIs(t, res.Err, ErrSpotifyGone, "no grace period, unlike a Spotify that went away")
	assert.Equal(t, OutcomeIncomplete, res.Outcome)
	assert.Equal(t, 1, res.Counts.Remaining)
	warnings := messages(s.eventsOf(EventWarning))
	assert.NotContains(t, warnings, "cannot reach Spotify: "+spotify.ErrNotAuthorized.Error())
	// Spotify no longer answers, so the shuffle cannot be put back yet: a
	// warning, and the next session finishes the job.
	assert.Contains(t, warnings, "could not restore Spotify settings: "+spotify.ErrNotAuthorized.Error())
	assert.False(t, s.spotify.shuffle)
}

func TestPendingAutomationPromptIsAWarning(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.at[2*time.Second] = func(s *sim) {
		s.spotify.user(func(f *fakeSpotify) { f.failNext, f.failErr = 3, spotify.ErrPromptPending })
	}

	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, []string{"macOS is asking whether mixtape may control Spotify: approve the prompt"},
		messages(s.eventsOf(EventWarning)))
}

func TestSpotifyRefusingACommandIsAWarning(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	s.spotify.refuse["play"], s.spotify.refuse["pause"] = errRefused, errRefused

	_, res := s.run(singles(songA), Options{StartTimeout: time.Second}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded, "the song is requested again once the start timeout passes")
	assert.Equal(t, 2, s.spotify.plays(0))
	warnings := messages(s.eventsOf(EventWarning))
	assert.Contains(t, warnings, "spotify play: "+errRefused.Error())
	assert.Contains(t, warnings, "spotify pause: "+errRefused.Error())
}

func TestAdBreakDoesNotCountAgainstTheSongsStart(t *testing.T) {
	t.Parallel()
	longAd := simTrack{id: "adlong", name: "Advertisement", dur: 5 * time.Second, ad: true}
	s := newSim(t, songA, longAd)
	s.spotify.adBreak = longAd.id // longer than the start timeout

	// One setback would end the session: the ad must not be one.
	_, res := s.run(singles(songA), Options{StartTimeout: 2 * time.Second, MaxFailures: 1}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	assert.Equal(t, 1, s.spotify.plays(0), "the song is not requested again while the ad plays")
	assert.Empty(t, s.eventsOf(EventTrackFailed))
}
