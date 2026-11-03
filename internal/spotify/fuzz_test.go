package spotify

import (
	"errors"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzParseRef checks that any pasted text either fails with one of the
// two link errors or yields a valid Ref that its own URI and URL parse
// back to. The ID ends up inside AppleScript source, so "valid" is the
// security property here, not a nicety.
func FuzzParseRef(f *testing.F) {
	for in := range parseRefAccepted() {
		f.Add(in)
	}
	for _, in := range parseRefRejected() {
		f.Add(in)
	}
	for in := range parseRefUnsupported() {
		f.Add(in)
	}
	for _, in := range likedSongsLinks() {
		f.Add(in)
	}

	f.Fuzz(func(t *testing.T, s string) {
		ref, err := ParseRef(s)
		if err != nil {
			require.True(t, errors.Is(err, ErrBadLink) || errors.Is(err, ErrUnsupportedLink),
				"unexpected error kind: %v", err)
			assert.Equal(t, Ref{}, ref)
			return
		}
		require.Contains(t, []Kind{KindTrack, KindAlbum, KindPlaylist}, ref.Kind)
		require.True(t, ValidID(ref.ID), "invalid ID %q from %q", ref.ID, s)
		for _, form := range []string{ref.URI(), ref.URL()} {
			back, err := ParseRef(form)
			require.NoError(t, err, form)
			assert.Equal(t, ref, back, form)
		}
	})
}

// FuzzEmbedPage feeds arbitrary embed pages through the parser and, for
// every link kind, the conversion into a Collection. Spotify's embed page
// is undocumented and may change at any time, so whatever it serves must
// either be refused with errEmbed (which falls back to oEmbed) or give a
// well-formed collection.
func FuzzEmbedPage(f *testing.F) {
	for _, name := range []string{"embed_album.html", "embed_missing.html", "embed_playlist.html", "embed_track.html"} {
		f.Add([]byte(fixture(f, name).body))
	}
	for _, entity := range []map[string]any{
		{
			"type": "track", "name": "Speak to Me", "duration": 64333,
			"releaseDate": map[string]any{"isoString": "1973-03-01T00:00:00Z"},
		},
		truncatedPlaylist(),
		{"type": "playlist", "name": "x"},
		{"type": "playlist", "name": "Road Trip"},
		{"type": "album", "name": "Empty", "trackList": []any{}},
		{"type": "playlist"},
	} {
		f.Add([]byte(embedHTML(f, entity)))
	}
	for _, body := range []string{
		"",
		"<html>redesigned</html>",
		nextDataStart + `{"props":`,
		nextDataStart + `not json` + nextDataEnd,
	} {
		f.Add([]byte(body))
	}

	refs := []Ref{{KindTrack, testTrackID}, {KindAlbum, testAlbumID}, {KindPlaylist, testPlaylistID}}
	f.Fuzz(func(t *testing.T, body []byte) {
		page, err := parseEmbedPage(body)
		if err != nil {
			require.ErrorIs(t, err, errEmbed)
			return
		}
		pp := page.Props.PageProps
		if pp.State == nil || pp.State.Data.Entity == nil {
			return // embed reports these before converting anything
		}
		for _, ref := range refs {
			col, err := collectionFromEntity(ref, pp.State.Data.Entity)
			if err != nil {
				require.ErrorIs(t, err, errEmbed, ref)
				continue
			}
			checkEmbedCollection(t, ref, col)
		}
	})
}

// checkEmbedCollection asserts what every collection read from an embed
// page must satisfy.
func checkEmbedCollection(t *testing.T, ref Ref, col Collection) {
	t.Helper()
	assert.Equal(t, ref, col.Ref)
	assert.Equal(t, SourceEmbed, col.Source)
	assert.NotEmpty(t, col.Name, "a collection always has a name")
	for i, tr := range col.Tracks {
		require.Equal(t, i+1, tr.Position, "positions run 1..n: %+v", col.Tracks)
		require.True(t, ValidID(tr.ID), "track %d has an invalid ID %q", i, tr.ID)
	}
	switch ref.Kind {
	case KindTrack:
		require.Len(t, col.Tracks, 1)
		assert.Equal(t, ref.ID, col.Tracks[0].ID, "a track link lists that track")
		assert.True(t, col.Complete)
		assert.False(t, col.Truncated)
	case KindPlaylist:
		assert.Equal(t, !col.Truncated, col.Complete, "a playlist is complete unless truncated")
	case KindAlbum:
		assert.True(t, col.Complete, "the page lists a whole album")
		assert.False(t, col.Truncated, "only playlists are truncated")
	}
}

// FuzzParseSnapshot checks the decoding of one line of the AppleScript
// snapshot output. The line carries text Spotify got from track metadata,
// so it must never panic, and a line is either an error or a snapshot,
// never half of each.
func FuzzParseSnapshot(f *testing.F) {
	for _, line := range []string{
		snapLine("playing", "spotify:track:"+testTrackID, "Never Gonna Give You Up", "Rick Astley", "12.345"),
		snapLine("stopped", "", "", "", "missing value"),
		snapLine("playing", "spotify:ad:000000012c91b4e1000000203d88d1ef", "Spotify", "", "1"),
		snapLine("playing", "", "Advertisement", "", "1"),
		scriptLine(markSnapshot, "paused", "100", "true", "false", "", "", "", "", "", "0", "0", "0", "0", "", "3,5"),
		scriptLine(markSnapshot, "playing", "80", "false", "false", "spotify:track:"+testTrackID, "Song", "missing value",
			"missing value", "missing value", "1000", "1", "1", "0", "", "1.5"),
		// A duration past time.Duration's range once wrapped to a negative one.
		scriptLine(markSnapshot, "playing", "80", "false", "false", "spotify:track:"+testTrackID, "Song", "", "", "",
			"1e13", "1e12", "-1e12", "1", "", "1e308"),
		markNotRunning,
		scriptLine(markError, "-1743", "Not authorized."),
		scriptLine(markError, "-600", "isn't running"),
		scriptLine(markError, "-1728", "Can't get name."),
		scriptLine(markError, "x", "odd"),
		scriptLine(markError, "0", "no number"),
		scriptLine(markError, "-1743", ""),
		scriptLine(markError, "-1712"),
		"hello",
		scriptLine(markSnapshot, "playing"),
		"",
	} {
		f.Add(line)
	}

	// The fake runner and the stubbed process check keep the fuzzer away
	// from osascript and the real process table.
	p := NewPlayer(&fakeRunner{})
	p.isRunning = func() (bool, error) { return false, nil }
	at := time.Unix(1_700_000_000, 0)

	f.Fuzz(func(t *testing.T, line string) {
		s := p.parseSnapshot(line, at)
		require.Equal(t, at, s.At)
		if s.Err != nil {
			assert.Equal(t, Snapshot{At: at, Position: -1, Err: s.Err}, s, "a failed observation carries nothing else")
			assert.True(t, isSnapshotError(s.Err), "unexpected error kind: %v", s.Err)
			return
		}

		trimmed := strings.TrimSpace(line)
		require.True(t, strings.HasPrefix(trimmed, markSnapshot+fieldSep), "only an S line parses: %q", line)
		require.Equal(t, fieldCount, strings.Count(trimmed, fieldSep), "only a full S line parses: %q", line)

		assert.False(t, math.IsNaN(s.Position) || math.IsInf(s.Position, 0), "position %v", s.Position)
		assert.GreaterOrEqual(t, s.Track.Duration, time.Duration(0), "duration %v", s.Track.Duration)
		for name, v := range map[string]int{
			"volume": s.Volume, "track number": s.Track.TrackNumber,
			"disc number": s.Track.DiscNumber, "popularity": s.Track.Popularity,
		} {
			assert.True(t, v >= math.MinInt32 && v <= math.MaxInt32, "%s %d is out of range", name, v)
		}
		if s.Track.ID != "" {
			require.True(t, ValidID(s.Track.ID), "invalid track ID %q", s.Track.ID)
			assert.Equal(t, TrackURI(s.Track.ID), s.Track.URI)
		}
	})
}

// isSnapshotError reports whether err is one parseSnapshot may return.
func isSnapshotError(err error) bool {
	return slices.ContainsFunc([]error{
		errBadSnapshot, ErrNotRunning, ErrNotAuthorized, ErrAppleEventsBlocked, ErrPromptPending, ErrScript,
	}, func(target error) bool { return errors.Is(err, target) })
}

// FuzzParsePrefs checks that any prefs file content gives a usable
// crossfade length. Spotify owns the format and may write anything.
func FuzzParsePrefs(f *testing.F) {
	for _, body := range []string{
		"app.player.volume=31270\naudio.gapless_migrated_to_ps=true\naudio.crossfade_v2=true\naudio.crossfade.time_v2=6658\n",
		`audio.automix_enabled="true"` + "\n",
		"audio.crossfade_migrated_to_ps=true\naudio.crossfade_v2=true\naudio.crossfade.time_v2=6658\n",
		"garbage\n audio.crossfade_v2 = true \naudio.crossfade.time_v2=nope\nui.automix=false\n",
		"audio.crossfade.time_v2=-5\n",
		// ParseFloat accepts "Inf", which once made the crossfade infinite.
		"audio.crossfade_v2=true\naudio.crossfade.time_v2=Inf\n",
		"",
	} {
		f.Add([]byte(body))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		p := parsePrefs(data)
		assert.False(t, math.IsNaN(p.CrossfadeSeconds) || math.IsInf(p.CrossfadeSeconds, 0),
			"crossfade %v s", p.CrossfadeSeconds)
		assert.GreaterOrEqual(t, p.CrossfadeSeconds, 0.0)
		assert.Empty(t, p.Path, "only ReadPrefs knows the path")
	})
}
