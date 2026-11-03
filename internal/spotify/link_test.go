package spotify

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testTrackID    = "4cOdK2wGLETKBW3PvgPWqT"
	testAlbumID    = "4LH4d3cOWNNsVw41Gqt2kv"
	testPlaylistID = "37i9dQZF1DXcBWIGoYBM5M"
)

// parseRefAccepted maps links and URIs ParseRef accepts to their Ref.
// FuzzParseRef seeds from it too.
func parseRefAccepted() map[string]Ref {
	return map[string]Ref{
		"https://open.spotify.com/track/" + testTrackID:                 {KindTrack, testTrackID},
		"https://open.spotify.com/track/" + testTrackID + "?si=abc123":  {KindTrack, testTrackID},
		"https://open.spotify.com/track/" + testTrackID + "#t=30":       {KindTrack, testTrackID},
		"https://open.spotify.com/track/" + testTrackID + "/":           {KindTrack, testTrackID},
		"http://open.spotify.com/album/" + testAlbumID:                  {KindAlbum, testAlbumID},
		"https://open.spotify.com/intl-de/album/" + testAlbumID:         {KindAlbum, testAlbumID},
		"https://OPEN.spotify.com/playlist/" + testPlaylistID + "?si=x": {KindPlaylist, testPlaylistID},
		"open.spotify.com/playlist/" + testPlaylistID:                   {KindPlaylist, testPlaylistID},
		"https://play.spotify.com/track/" + testTrackID:                 {KindTrack, testTrackID},
		"https://open.spotify.com/user/someone/playlist/" + testPlaylistID: {
			KindPlaylist, testPlaylistID,
		},
		"spotify:track:" + testTrackID:                             {KindTrack, testTrackID},
		"spotify:album:" + testAlbumID:                             {KindAlbum, testAlbumID},
		"spotify:playlist:" + testPlaylistID:                       {KindPlaylist, testPlaylistID},
		"spotify:user:someone:playlist:" + testPlaylistID:          {KindPlaylist, testPlaylistID},
		"  <https://open.spotify.com/track/" + testTrackID + ">  ": {KindTrack, testTrackID},
		`"spotify:track:` + testTrackID + `"`:                      {KindTrack, testTrackID},
		"'spotify:TRACK:" + testTrackID + "'":                      {KindTrack, testTrackID},
	}
}

func TestParseRef(t *testing.T) {
	t.Parallel()
	for in, want := range parseRefAccepted() {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got, err := ParseRef(in)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}
}

// parseRefRejected lists text ParseRef rejects as ErrBadLink.
func parseRefRejected() []string {
	return []string{
		"",
		"   ",
		"Still Loving You",
		"https://example.com/track/" + testTrackID,
		"ftp://open.spotify.com/track/" + testTrackID,
		"https://open.spotify.com/",
		"https://open.spotify.com/track/",
		"https://open.spotify.com/track/abc/def",
		"https://open.spotify.com/track/abc-def",
		`https://open.spotify.com/track/x" & (do shell script "id") & "`,
		"spotify:",
		"spotify:track:",
		"spotify:track:a b",
		"spotify:track:abc:extra",
		"spotify:banana:" + testTrackID,
		"https://open.spotify.com/banana/" + testTrackID,
		"https://open.spotify.com/track/" + strings.Repeat("a", 65),
		"%zz",
		"https://open.spotify.com/%zz",
	}
}

func TestParseRefRejects(t *testing.T) {
	t.Parallel()
	for _, in := range parseRefRejected() {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRef(in)
			require.ErrorIs(t, err, ErrBadLink)
		})
	}
}

// parseRefUnsupported maps links ParseRef rejects as ErrUnsupportedLink
// to a word its message must contain.
func parseRefUnsupported() map[string]string {
	return map[string]string{
		"https://open.spotify.com/episode/" + testTrackID: "episode",
		"https://open.spotify.com/show/" + testTrackID:    "show",
		"https://open.spotify.com/artist/" + testTrackID:  "artist",
		"https://open.spotify.com/user/someone":           "user",
		"spotify:local:Artist:Album:Title:180":            "local",
		"spotify:episode:" + testTrackID:                  "episode",
		"spotify:ad:" + testTrackID:                       "ads",
		"spotify:artist:" + testTrackID:                   "artist",
	}
}

func TestParseRefUnsupported(t *testing.T) {
	t.Parallel()
	for in, want := range parseRefUnsupported() {
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			_, err := ParseRef(in)
			require.ErrorIs(t, err, ErrUnsupportedLink)
			assert.Contains(t, err.Error(), want)
		})
	}
}

func TestRefFormatting(t *testing.T) {
	t.Parallel()
	r := Ref{Kind: KindPlaylist, ID: testPlaylistID}
	assert.Equal(t, "spotify:playlist:"+testPlaylistID, r.URI())
	assert.Equal(t, r.URI(), r.String())
	assert.Equal(t, "https://open.spotify.com/playlist/"+testPlaylistID, r.URL())
	assert.Equal(t, "spotify:track:"+testTrackID, TrackURI(testTrackID))
	assert.Equal(t, "https://open.spotify.com/track/"+testTrackID, TrackURL(testTrackID))

	back, err := ParseRef(r.URL())
	require.NoError(t, err)
	assert.Equal(t, r, back)
}

func TestValidID(t *testing.T) {
	t.Parallel()
	assert.True(t, ValidID(testTrackID))
	assert.True(t, ValidID("a"))
	assert.False(t, ValidID(""))
	assert.False(t, ValidID("abc def"))
	assert.False(t, ValidID(`abc"`))
	assert.False(t, ValidID("ünï"))
	assert.False(t, ValidID(strings.Repeat("a", maxIDLen+1)))
	assert.True(t, ValidID(strings.Repeat("a", maxIDLen)))
}

func TestParseTrackURI(t *testing.T) {
	t.Parallel()
	assert.Equal(t, testTrackID, parseTrackURI("spotify:track:"+testTrackID))
	assert.Empty(t, parseTrackURI("spotify:ad:"+testTrackID))
	assert.Empty(t, parseTrackURI("spotify:local:a:b:c:1"))
	assert.Empty(t, parseTrackURI("spotify:track:"))
	assert.Empty(t, parseTrackURI(""))
}

// likedSongsLinks are links into the listener's own library.
func likedSongsLinks() []string {
	return []string{
		"https://open.spotify.com/collection/tracks",
		"open.spotify.com/collection/tracks?si=abc",
		"spotify:collection:tracks",
		"spotify:user:someone:collection",
		"spotify:user:someone:collection:tracks",
	}
}

func TestParseRefLikedSongsIsUnsupported(t *testing.T) {
	t.Parallel()
	for _, in := range likedSongsLinks() {
		_, err := ParseRef(in)
		require.ErrorIs(t, err, ErrUnsupportedLink, in)
		assert.Contains(t, err.Error(), "Liked Songs", in)
	}
}
