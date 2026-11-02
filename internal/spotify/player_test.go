package spotify

import (
	"context"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeRunner records scripts and answers from canned functions. It never
// runs osascript.
type fakeRunner struct {
	mu      sync.Mutex
	scripts []string
	args    [][]string
	run     func(script string) (string, error)
	starts  []func(ctx context.Context) (<-chan Line, error)
}

func (f *fakeRunner) Run(_ context.Context, script string, args ...string) (string, error) {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	f.args = append(f.args, args)
	run := f.run
	f.mu.Unlock()
	if run == nil {
		return markOK, nil
	}
	return run(script)
}

func (f *fakeRunner) Start(ctx context.Context, script string, args ...string) (<-chan Line, error) {
	f.mu.Lock()
	f.scripts = append(f.scripts, script)
	f.args = append(f.args, args)
	var next func(context.Context) (<-chan Line, error)
	if len(f.starts) > 0 {
		next, f.starts = f.starts[0], f.starts[1:]
	}
	f.mu.Unlock()
	if next == nil {
		ch := make(chan Line)
		go func() { <-ctx.Done(); close(ch) }()
		return ch, nil
	}
	return next(ctx)
}

func (f *fakeRunner) lastScript() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.scripts) == 0 {
		return ""
	}
	return f.scripts[len(f.scripts)-1]
}

// newTestPlayer returns a Player over r whose process check reports
// running and whose clock is fixed.
func newTestPlayer(r Runner, running bool) *Player {
	p := NewPlayer(r)
	p.isRunning = func() (bool, error) { return running, nil }
	p.now = func() time.Time { return time.Unix(1_700_000_000, 0) }
	p.backoffMin, p.backoffMax = time.Millisecond, 2*time.Millisecond
	return p
}

// scriptLine builds a line the way the AppleScript handler does: a
// marker, then its fields, all separated by fieldSep.
func scriptLine(marker string, fields ...string) string {
	return strings.Join(append([]string{marker}, fields...), fieldSep)
}

// snapLine builds the snapshot line of a typical track, with the fields
// tests vary given.
func snapLine(state, uri, name, artist, pos string) string {
	return scriptLine(markSnapshot,
		state, "80", "false", "true",
		uri, name, artist, "Whenever You Need Somebody", "Rick Astley",
		"213573", "1", "1", "77", "https://i.scdn.co/image/abc", pos,
	)
}

func TestPollParsesSnapshot(t *testing.T) {
	t.Parallel()
	line := snapLine("playing", "spotify:track:"+testTrackID, "Never Gonna Give You Up", "Rick Astley", "12.345")
	p := newTestPlayer(&fakeRunner{run: func(string) (string, error) { return line, nil }}, true)

	s := p.Poll(t.Context())
	require.NoError(t, s.Err)
	assert.Equal(t, time.Unix(1_700_000_000, 0), s.At)
	assert.Equal(t, StatePlaying, s.State)
	assert.InDelta(t, 12.345, s.Position, 1e-9)
	assert.Equal(t, 80, s.Volume)
	assert.False(t, s.Shuffle)
	assert.True(t, s.Repeat)
	assert.Equal(t, Track{
		ID:          testTrackID,
		URI:         "spotify:track:" + testTrackID,
		Name:        "Never Gonna Give You Up",
		Artist:      "Rick Astley",
		Album:       "Whenever You Need Somebody",
		AlbumArtist: "Rick Astley",
		Duration:    213573 * time.Millisecond,
		TrackNumber: 1,
		DiscNumber:  1,
		Popularity:  77,
		ArtworkURL:  "https://i.scdn.co/image/abc",
	}, s.Track)
	assert.False(t, s.Track.IsAd())
}

func TestPollLocaleAndEmptyTrack(t *testing.T) {
	t.Parallel()
	line := scriptLine(markSnapshot, "paused", "100", "true", "false", "", "", "", "", "", "0", "0", "0", "0", "", "3,5")
	p := newTestPlayer(&fakeRunner{run: func(string) (string, error) { return line, nil }}, true)

	s := p.Poll(t.Context())
	require.NoError(t, s.Err)
	assert.Equal(t, StatePaused, s.State)
	assert.InDelta(t, 3.5, s.Position, 1e-9, "comma decimal separator")
	assert.True(t, s.Shuffle)
	assert.Equal(t, Track{}, s.Track)
}

func TestPollMissingTextFieldsAreEmpty(t *testing.T) {
	t.Parallel()
	line := scriptLine(markSnapshot, "playing", "80", "false", "false", "spotify:track:"+testTrackID, "Song",
		"missing value", "missing value", "missing value", "1000", "1", "1", "0", "", "1.5")
	p := newTestPlayer(&fakeRunner{run: func(string) (string, error) { return line, nil }}, true)
	s := p.Poll(t.Context())
	require.NoError(t, s.Err)
	assert.Equal(t, "Song", s.Track.Name)
	assert.Empty(t, s.Track.Artist)
	assert.Empty(t, s.Track.Album)
	assert.Empty(t, s.Track.AlbumArtist)
}

func TestSnapshotScriptKeepsEachSnapshotOnOneLine(t *testing.T) {
	t.Parallel()
	assert.Contains(t, snapshotHandler, "my mixtapeClean(name of mTrack)", "names are cleaned of line breaks")
	assert.Contains(t, snapshotHandler, "repeat with c in {linefeed, return, character id 31}")
}

func TestPollMissingPosition(t *testing.T) {
	t.Parallel()
	line := snapLine("stopped", "", "", "", "missing value")
	p := newTestPlayer(&fakeRunner{run: func(string) (string, error) { return line, nil }}, true)
	s := p.Poll(t.Context())
	require.NoError(t, s.Err)
	assert.Negative(t, s.Position)
}

func TestPollAds(t *testing.T) {
	t.Parallel()
	for _, line := range []string{
		snapLine("playing", "spotify:ad:000000012c91b4e1000000203d88d1ef", "Spotify", "", "1"),
		snapLine("playing", "", "Advertisement", "", "1"),
	} {
		p := newTestPlayer(&fakeRunner{run: func(string) (string, error) { return line, nil }}, true)
		s := p.Poll(t.Context())
		require.NoError(t, s.Err)
		assert.Empty(t, s.Track.ID)
		assert.True(t, s.Track.IsAd())
	}
}

// osascriptError is the error OSAScript.Run returns when the script fails
// with code and msg.
func osascriptError(code int, msg string) *ScriptError {
	return &ScriptError{Code: code, Msg: fmt.Sprintf("osascript: exit status 1: 0:42: execution error: %s (%d)", msg, code)}
}

func TestPollErrorKeepsTheScriptsText(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{run: func(string) (string, error) {
		return scriptLine(markError, "-1743", "Not authorized to send Apple events to Spotify."), nil
	}}
	s := newTestPlayer(r, false).Poll(t.Context())
	require.ErrorIs(t, s.Err, ErrNotAuthorized)
	assert.Equal(t, ErrNotAuthorized.Error()+": Not authorized to send Apple events to Spotify. (-1743)", s.Err.Error())
}

func TestPollErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		out     string
		err     error
		running bool
		want    error
	}{
		"not running marker":   {out: markNotRunning, want: ErrNotRunning},
		"not authorized":       {out: scriptLine(markError, "-1743", "Not authorized."), want: ErrNotAuthorized},
		"proc gone, stopped":   {out: scriptLine(markError, "-600", "isn't running"), want: ErrNotRunning},
		"proc gone, running":   {out: scriptLine(markError, "-600", "isn't running"), running: true, want: ErrAppleEventsBlocked},
		"other script error":   {out: scriptLine(markError, "-1728", "Can't get name."), want: ErrScript},
		"error without code":   {out: scriptLine(markError, "x", "odd"), want: ErrScript},
		"garbage":              {out: "hello", want: errBadSnapshot},
		"short":                {out: scriptLine(markSnapshot, "playing"), want: errBadSnapshot},
		"runner denied":        {err: osascriptError(-1743, "Not authorized to send Apple events to Spotify."), want: ErrNotAuthorized},
		"runner timeout":       {err: fmt.Errorf("osascript: %w", context.DeadlineExceeded), want: ErrPromptPending},
		"runner sandboxed":     {err: osascriptError(-10810, "An error of type -10810 has occurred."), running: true, want: ErrAppleEventsBlocked},
		"runner not running":   {err: osascriptError(-600, "Spotify got an error: Application isn't running."), want: ErrNotRunning},
		"runner plain failure": {err: textError("exit status 1"), want: ErrScript},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{run: func(string) (string, error) { return tc.out, tc.err }}
			s := newTestPlayer(r, tc.running).Poll(t.Context())
			require.ErrorIs(t, s.Err, tc.want)
		})
	}
}

func TestCommands(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		call func(p *Player, ctx context.Context) error
		want string
	}{
		"play":         {func(p *Player, ctx context.Context) error { return p.Play(ctx) }, "\t\tplay\n"},
		"pause":        {func(p *Player, ctx context.Context) error { return p.Pause(ctx) }, "\t\tpause\n"},
		"next":         {func(p *Player, ctx context.Context) error { return p.Next(ctx) }, "\t\tnext track\n"},
		"playpause":    {func(p *Player, ctx context.Context) error { return p.PlayPause(ctx) }, "\t\tplaypause\n"},
		"seek":         {func(p *Player, ctx context.Context) error { return p.SetPosition(ctx, 12.5) }, "set player position to 12.500"},
		"seek to zero": {func(p *Player, ctx context.Context) error { return p.SetPosition(ctx, 0) }, "set player position to 0.000"},
		"shuffle on":   {func(p *Player, ctx context.Context) error { return p.SetShuffle(ctx, true) }, "set shuffling to true"},
		"repeat off":   {func(p *Player, ctx context.Context) error { return p.SetRepeat(ctx, false) }, "set repeating to false"},
		"volume":       {func(p *Player, ctx context.Context) error { return p.SetVolume(ctx, 55) }, "set sound volume to 55"},
		"volume high":  {func(p *Player, ctx context.Context) error { return p.SetVolume(ctx, 250) }, "set sound volume to 100"},
		"volume low":   {func(p *Player, ctx context.Context) error { return p.SetVolume(ctx, -5) }, "set sound volume to 0"},
		"play track": {
			func(p *Player, ctx context.Context) error { return p.PlayTrack(ctx, "spotify:track:"+testTrackID, "") },
			`play track "spotify:track:` + testTrackID + `"` + "\n",
		},
		"play in context": {
			func(p *Player, ctx context.Context) error {
				return p.PlayTrack(ctx, "spotify:track:"+testTrackID, "spotify:playlist:"+testPlaylistID)
			},
			`play track "spotify:track:` + testTrackID + `" in context "spotify:playlist:` + testPlaylistID + `"`,
		},
		"play a playlist": {
			func(p *Player, ctx context.Context) error {
				return p.PlayTrack(ctx, "spotify:playlist:"+testPlaylistID, "")
			},
			`play track "spotify:playlist:` + testPlaylistID + `"`,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := &fakeRunner{}
			require.NoError(t, tc.call(newTestPlayer(r, true), t.Context()))
			script := r.lastScript()
			assert.Contains(t, script, tc.want)
			assert.Contains(t, script, `if application "Spotify" is not running then return "N"`,
				"commands must never launch Spotify")
		})
	}
}

func TestCommandResults(t *testing.T) {
	t.Parallel()
	notRunning := &fakeRunner{run: func(string) (string, error) { return markNotRunning, nil }}
	require.ErrorIs(t, newTestPlayer(notRunning, false).Next(t.Context()), ErrNotRunning)

	denied := &fakeRunner{run: func(string) (string, error) {
		return "", osascriptError(-1743, "Not authorized to send Apple events to Spotify.")
	}}
	require.ErrorIs(t, newTestPlayer(denied, true).Pause(t.Context()), ErrNotAuthorized)

	odd := &fakeRunner{run: func(string) (string, error) { return "what", nil }}
	require.ErrorIs(t, newTestPlayer(odd, true).Play(t.Context()), errBadSnapshot)
}

func TestPlayTrackRejectsUnsafeURIs(t *testing.T) {
	t.Parallel()
	for _, uri := range []string{
		"",
		"spotify:track:",
		"spotify:artist:" + testTrackID,
		"spotify:episode:" + testTrackID,
		`spotify:track:x" & (do shell script "id") & "`,
		"spotify:track:abc def",
		"https://open.spotify.com/track/" + testTrackID,
		"spotify:track:a:b",
	} {
		r := &fakeRunner{}
		p := newTestPlayer(r, true)
		require.ErrorIs(t, p.PlayTrack(t.Context(), uri, ""), errBadURI, uri)
		require.ErrorIs(t, p.PlayTrack(t.Context(), "spotify:track:"+testTrackID, uri+"x\""), errBadURI, uri)
		assert.Empty(t, r.lastScript(), "nothing may reach osascript for %q", uri)
	}
}

func TestSetPositionRejects(t *testing.T) {
	t.Parallel()
	r := &fakeRunner{}
	p := newTestPlayer(r, true)
	for _, v := range []float64{-1, math.NaN(), math.Inf(1)} {
		require.ErrorIs(t, p.SetPosition(t.Context(), v), errBadValue)
	}
	assert.Empty(t, r.lastScript())
}

func TestScriptsNeverLaunchSpotify(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]string{"poll": pollScript, "watch": watchScript, "command": commandScript} {
		assert.Contains(t, s, `if application "Spotify" is not running then return "N"`, name)
	}
	// Position must be the last field read, so its sample time is the
	// line's arrival time.
	assert.Less(t, strings.Index(snapshotHandler, "set mArtwork to artwork url"),
		strings.Index(snapshotHandler, "set mPosition to player position"))
	assert.Equal(t, fieldCount-1, fPosition)
}

// lines returns a Start function that emits texts, then exits with err.
func lines(texts []string, err error) func(context.Context) (<-chan Line, error) {
	return func(ctx context.Context) (<-chan Line, error) {
		ch := make(chan Line, len(texts)+1)
		go func() {
			defer close(ch)
			for i, text := range texts {
				select {
				case ch <- Line{Text: text, At: time.Unix(int64(i), 0)}:
				case <-ctx.Done():
					return
				}
			}
			ch <- Line{At: time.Unix(99, 0), Err: err}
		}()
		return ch, nil
	}
}

func TestWatchStreamsAndRestarts(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	a := snapLine("playing", "spotify:track:"+testTrackID, "A", "X", "1.0")
	b := snapLine("playing", "spotify:track:"+testTrackID, "A", "X", "1.2")
	r := &fakeRunner{starts: []func(context.Context) (<-chan Line, error){
		lines([]string{a, "2026-11-02 osascript[1:2] Connection Invalid error for service", b},
			textError("osascript: exit status 1: boom")),
		func(context.Context) (<-chan Line, error) { return nil, textError("cannot start") },
		lines([]string{markNotRunning}, errNoOutput),
	}}
	p := newTestPlayer(r, true)
	ch := p.Watch(ctx, 100*time.Millisecond)

	got := make([]Snapshot, 0, 6)
	for s := range ch {
		got = append(got, s)
		if len(got) == 6 {
			cancel()
		}
	}
	require.GreaterOrEqual(t, len(got), 6)
	require.NoError(t, got[0].Err)
	assert.InDelta(t, 1.0, got[0].Position, 1e-9)
	assert.Equal(t, time.Unix(0, 0), got[0].At, "At is the line's arrival time")
	require.NoError(t, got[1].Err, "noise lines are skipped")
	assert.InDelta(t, 1.2, got[1].Position, 1e-9)
	require.ErrorIs(t, got[2].Err, ErrScript, "process exit is reported")
	require.ErrorIs(t, got[3].Err, ErrScript, "a failed start is reported")
	require.ErrorIs(t, got[4].Err, ErrNotRunning)
	require.Error(t, got[5].Err)

	r.mu.Lock()
	defer r.mu.Unlock()
	assert.Equal(t, []string{"100"}, r.args[0], "interval passed in milliseconds")
	assert.Contains(t, r.scripts[0], "repeat")
}

func TestWatchClassifiesTheScriptsExit(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	r := &fakeRunner{starts: []func(context.Context) (<-chan Line, error){
		lines(nil, osascriptError(-600, "Spotify got an error: Application isn't running.")),
	}}
	s := <-newTestPlayer(r, true).Watch(ctx, time.Millisecond)
	require.ErrorIs(t, s.Err, ErrAppleEventsBlocked, "-600 while Spotify runs means Apple Events are blocked")
}

func TestConsumeStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	p := newTestPlayer(&fakeRunner{}, true)
	snap := snapLine("playing", "spotify:track:"+testTrackID, "A", "X", "1.0")

	exited := make(chan Line, 1)
	exited <- Line{Err: textError("signal: killed")}
	close(exited)
	out := make(chan Snapshot, 1)
	p.consume(ctx, exited, out)
	assert.Empty(t, out, "the exit of a canceled watch is not reported")

	pending := make(chan Line, 2)
	pending <- Line{Text: snap}
	pending <- Line{Text: snap}
	close(pending)
	p.consume(ctx, pending, make(chan Snapshot))
	assert.Len(t, pending, 1, "it stops at the first snapshot nobody can receive")
}

func TestWatchClosesOnCancel(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	p := newTestPlayer(&fakeRunner{}, true)
	ch := p.Watch(ctx, time.Millisecond)
	cancel()
	select {
	case _, ok := <-ch:
		for ok {
			_, ok = <-ch
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Watch did not close after cancel")
	}
}

func TestNewPlayerDefaults(t *testing.T) {
	t.Parallel()
	p := NewPlayer(nil)
	_, ok := p.run.(*OSAScript)
	assert.True(t, ok)
	assert.Equal(t, watchBackoffMin, p.backoffMin)
}

func TestNumberParsing(t *testing.T) {
	t.Parallel()
	v, ok := parseReal(" 1,25 ")
	assert.True(t, ok)
	assert.InDelta(t, 1.25, v, 1e-9)
	for _, s := range []string{"", "missing value", "abc", "NaN", "Inf"} {
		_, ok = parseReal(s)
		assert.False(t, ok, s)
	}
	assert.Equal(t, 3, atoi("3.0"))
	assert.Equal(t, 0, atoi("x"))
	assert.Equal(t, 0, atoi("1e12"))
}
