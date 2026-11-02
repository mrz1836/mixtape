package spotify

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// catalogServer serves embed pages from a path → (status, body) table and
// oEmbed answers from oembed (status, body). It records user agents and
// oEmbed url parameters.
type catalogServer struct {
	*httptest.Server

	embeds      map[string]page
	oembed      page
	oembedURL   atomic.Value
	userAgent   atomic.Value
	oembedCalls atomic.Int32
}

type page struct {
	status int
	body   string
}

func newCatalogServer(t *testing.T, embeds map[string]page, oembed page) (*catalogServer, *Catalog) {
	t.Helper()
	cs := &catalogServer{embeds: embeds, oembed: oembed}
	mux := http.NewServeMux()
	mux.HandleFunc("/embed/", func(w http.ResponseWriter, r *http.Request) {
		cs.userAgent.Store(r.UserAgent())
		p, ok := cs.embeds[strings.TrimPrefix(r.URL.Path, "/embed/")]
		if !ok {
			p = page{status: http.StatusInternalServerError}
		}
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	})
	mux.HandleFunc("/oembed", func(w http.ResponseWriter, r *http.Request) {
		cs.oembedCalls.Add(1)
		cs.oembedURL.Store(r.URL.Query().Get("url"))
		w.WriteHeader(cs.oembed.status)
		_, _ = w.Write([]byte(cs.oembed.body))
	})
	cs.Server = httptest.NewServer(mux)
	t.Cleanup(cs.Close)
	c := &Catalog{
		Client:     cs.Client(),
		EmbedBase:  cs.URL + "/embed",
		OEmbedBase: cs.URL + "/oembed",
		UserAgent:  "mixtape-test",
	}
	return cs, c
}

func fixture(t *testing.T, name string) page {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return page{status: http.StatusOK, body: string(b)}
}

// embedHTML wraps an entity the way the embed page does.
func embedHTML(t *testing.T, entity any) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"props": map[string]any{"pageProps": map[string]any{"state": map[string]any{
			"data": map[string]any{"entity": entity},
		}}},
	})
	require.NoError(t, err)
	return `<html><body><script id="__NEXT_DATA__" type="application/json">` + string(b) + `</script></body></html>`
}

func TestResolvePlaylist(t *testing.T) {
	t.Parallel()
	cs, c := newCatalogServer(t, map[string]page{
		"playlist/" + testPlaylistID: fixture(t, "embed_playlist.html"),
	}, page{status: http.StatusOK})

	col, err := c.Resolve(t.Context(), Ref{KindPlaylist, testPlaylistID})
	require.NoError(t, err)
	assert.Equal(t, "Today’s Top Hits", col.Name)
	assert.Equal(t, "Spotify", col.Owner)
	assert.Equal(t, SourceEmbed, col.Source)
	assert.True(t, col.Complete)
	assert.False(t, col.Truncated)
	assert.True(t, strings.HasPrefix(col.ArtworkURL, "https://"), col.ArtworkURL)
	require.Len(t, col.Tracks, 5)
	first := col.Tracks[0]
	assert.Equal(t, CatalogTrack{
		ID: "7bxaFZ1O3cHkgLKMsdC3xR", Title: "Loser", Artist: "Tame Impala",
		Duration: 223069 * time.Millisecond, Playable: true, Position: 1,
	}, first)
	for i, tr := range col.Tracks {
		assert.Equal(t, i+1, tr.Position)
		assert.True(t, ValidID(tr.ID))
	}
	assert.Equal(t, "mixtape-test", cs.userAgent.Load())
	assert.Zero(t, cs.oembedCalls.Load(), "no fallback when the embed page works")
}

func TestResolveAlbum(t *testing.T) {
	t.Parallel()
	_, c := newCatalogServer(t, map[string]page{
		"album/" + testAlbumID: fixture(t, "embed_album.html"),
	}, page{status: http.StatusOK})

	col, err := c.Resolve(t.Context(), Ref{KindAlbum, testAlbumID})
	require.NoError(t, err)
	assert.Equal(t, "The Dark Side of the Moon", col.Name)
	assert.Equal(t, "Pink Floyd", col.Owner)
	assert.Contains(t, col.ArtworkURL, "ab67616d0000b273", "the 640px image wins")
	require.Len(t, col.Tracks, 4)
	assert.Equal(t, "Speak to Me", col.Tracks[0].Title)
	assert.True(t, col.Complete)
}

func TestResolveTrack(t *testing.T) {
	t.Parallel()
	_, c := newCatalogServer(t, map[string]page{
		"track/" + testTrackID: fixture(t, "embed_track.html"),
	}, page{status: http.StatusOK})

	col, err := c.Resolve(t.Context(), Ref{KindTrack, testTrackID})
	require.NoError(t, err)
	assert.Equal(t, "Never Gonna Give You Up", col.Name)
	assert.Empty(t, col.Owner)
	require.Len(t, col.Tracks, 1)
	assert.Equal(t, CatalogTrack{
		ID: testTrackID, Title: "Never Gonna Give You Up", Artist: "Rick Astley",
		Duration: 213573 * time.Millisecond, Playable: true, Position: 1,
	}, col.Tracks[0])
	assert.True(t, col.Complete)
	assert.NotEmpty(t, col.ArtworkURL)
}

func TestResolveTruncatedAndFiltered(t *testing.T) {
	t.Parallel()
	items := make([]map[string]any, 0, 100)
	items = append(items,
		map[string]any{"uri": "spotify:local:Artist:Album:Title:180", "title": "Local", "duration": 180000},
		map[string]any{"uri": "spotify:episode:" + testTrackID, "title": "Pod", "duration": 1},
	)
	for i := len(items); i < 100; i++ {
		id := fmt.Sprintf("%022d", i)
		items = append(items, map[string]any{
			"uri": "spotify:track:" + id, "title": fmt.Sprintf("Song %d", i), "subtitle": "Artist",
			"duration": 200000, "isPlayable": i%10 != 0,
		})
	}
	body := embedHTML(t, map[string]any{"type": "playlist", "name": "Big", "subtitle": "someone", "trackList": items})
	_, c := newCatalogServer(t, map[string]page{"playlist/" + testPlaylistID: {http.StatusOK, body}}, page{})

	col, err := c.Resolve(t.Context(), Ref{KindPlaylist, testPlaylistID})
	require.NoError(t, err)
	assert.True(t, col.Truncated)
	assert.False(t, col.Complete)
	require.Len(t, col.Tracks, 98, "local files and episodes are dropped")
	assert.Equal(t, 1, col.Tracks[0].Position)
	assert.Equal(t, "Song 2", col.Tracks[0].Title)
	assert.False(t, col.Tracks[8].Playable, "Song 10 is flagged unplayable")
}

func TestResolveNotFound(t *testing.T) {
	t.Parallel()
	cs, c := newCatalogServer(t, map[string]page{
		"track/" + testTrackID: fixture(t, "embed_missing.html"),
		"album/" + testAlbumID: {status: http.StatusNotFound},
	}, page{status: http.StatusOK, body: `{"title":"should not be used"}`})

	_, err := c.Resolve(t.Context(), Ref{KindTrack, testTrackID})
	require.ErrorIs(t, err, ErrNotFound, "the embed page reports 404 inside a 200 page")
	_, err = c.Resolve(t.Context(), Ref{KindAlbum, testAlbumID})
	require.ErrorIs(t, err, ErrNotFound)
	assert.Zero(t, cs.oembedCalls.Load())
}

func TestResolveFallsBackToOEmbed(t *testing.T) {
	t.Parallel()
	broken := map[string]page{
		"playlist/" + testPlaylistID: {status: http.StatusOK, body: "<html>redesigned</html>"},
		"album/" + testAlbumID:       {status: http.StatusOK, body: embedHTML(t, map[string]any{"type": "playlist", "name": "x"})},
		// track: no entry → 500
	}
	cs, c := newCatalogServer(t, broken, page{status: http.StatusOK, body: `{"title":"From oEmbed","html":"<iframe>"}`})

	col, err := c.Resolve(t.Context(), Ref{KindPlaylist, testPlaylistID})
	require.NoError(t, err)
	assert.Equal(t, Collection{Ref: Ref{KindPlaylist, testPlaylistID}, Name: "From oEmbed", Source: SourceOEmbed}, col)
	assert.Equal(t, "https://open.spotify.com/playlist/"+testPlaylistID, cs.oembedURL.Load())

	col, err = c.Resolve(t.Context(), Ref{KindAlbum, testAlbumID})
	require.NoError(t, err, "an entity of the wrong type falls back")
	assert.Equal(t, SourceOEmbed, col.Source)

	col, err = c.Resolve(t.Context(), Ref{KindTrack, testTrackID})
	require.NoError(t, err)
	assert.False(t, col.Complete)
	require.Len(t, col.Tracks, 1)
	assert.Equal(t, CatalogTrack{ID: testTrackID, Title: "From oEmbed", Playable: true, Position: 1}, col.Tracks[0])
}

func TestResolveEmptyTrackListIsNotAnEmptyCollection(t *testing.T) {
	t.Parallel()
	pages := map[string]page{
		// The page still names the playlist, but its list is gone (renamed
		// key, or emptied): it must not read as a complete, empty playlist.
		"playlist/" + testPlaylistID: {status: http.StatusOK, body: embedHTML(t, map[string]any{"type": "playlist", "name": "Road Trip"})},
		"album/" + testAlbumID: {status: http.StatusOK, body: embedHTML(t, map[string]any{
			"type": "album", "name": "Empty", "trackList": []any{},
		})},
	}
	_, c := newCatalogServer(t, pages, page{status: http.StatusOK, body: `{"title":"Road Trip","html":"<iframe>"}`})

	for _, ref := range []Ref{{KindPlaylist, testPlaylistID}, {KindAlbum, testAlbumID}} {
		col, err := c.Resolve(t.Context(), ref)
		require.NoError(t, err, ref)
		assert.Equal(t, SourceOEmbed, col.Source, "falls back to following it in Spotify: %v", ref)
		assert.False(t, col.Complete, ref)
	}
}

func TestResolveOEmbedErrors(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		oembed page
		want   error
	}{
		"oembed 404":    {page{status: http.StatusNotFound}, ErrNotFound},
		"oembed 500":    {page{status: http.StatusInternalServerError}, ErrCatalog},
		"missing title": {page{status: http.StatusOK, body: `{}`}, ErrCatalog},
		"bad json":      {page{status: http.StatusOK, body: `{`}, ErrCatalog},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, c := newCatalogServer(t, map[string]page{}, tc.oembed)
			_, err := c.Resolve(t.Context(), Ref{KindTrack, testTrackID})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

func TestResolveUnreachable(t *testing.T) {
	t.Parallel()
	_, c := newCatalogServer(t, nil, page{})
	c.EmbedBase, c.OEmbedBase = "http://127.0.0.1:1/embed", "http://127.0.0.1:1/oembed"
	_, err := c.Resolve(t.Context(), Ref{KindTrack, testTrackID})
	require.ErrorIs(t, err, ErrCatalog)
}

func TestParseEmbedPageErrors(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		"",
		`<script id="__NEXT_DATA__" type="application/json">{"props":`,
		`<script id="__NEXT_DATA__" type="application/json">not json</script>`,
	} {
		_, err := parseEmbedPage([]byte(body))
		require.ErrorIs(t, err, errEmbed, body)
	}
}

func TestCollectionFromEntityNeedsName(t *testing.T) {
	t.Parallel()
	_, err := collectionFromEntity(Ref{KindPlaylist, testPlaylistID}, &embedEntity{Type: "playlist"})
	require.ErrorIs(t, err, errEmbed)
}

func TestLargestImage(t *testing.T) {
	t.Parallel()
	w := func(n int) *int { return &n }
	assert.Empty(t, largestImage(nil))
	assert.Equal(t, "a", largestImage([]embedImage{{URL: "a"}, {URL: "b"}}), "unsized: first listed")
	assert.Equal(t, "b", largestImage([]embedImage{{URL: "a", MaxWidth: w(64)}, {URL: "b", Width: w(640)}, {URL: ""}}))
}

func TestNewCatalogDefaults(t *testing.T) {
	t.Parallel()
	c := NewCatalog()
	assert.Equal(t, defaultEmbedBase, c.EmbedBase)
	assert.Equal(t, defaultOEmbedBase, c.OEmbedBase)
	assert.Contains(t, c.UserAgent, "Mozilla/5.0")
	require.NotNil(t, c.Client)
	assert.Equal(t, catalogTimeout, c.Client.Timeout)

	var zero Catalog
	assert.Equal(t, defaultEmbedBase, zero.embedBase())
}
