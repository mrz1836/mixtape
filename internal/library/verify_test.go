package library

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
)

// verifyFixture is a playlist of two recorded songs.
func verifyFixture(t *testing.T) (*fixture, *Collection) {
	t.Helper()
	requireAAC(t)
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	f.commit(c, trackTime(), 440, CommitOptions{})
	f.commit(c, trackMoney(), 550, CommitOptions{})
	return f, c
}

func kinds(ps []Problem) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, p.Kind)
	}
	return out
}

func TestVerifyClean(t *testing.T) {
	t.Parallel()
	f, _ := verifyFixture(t)
	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Collections)
	assert.Equal(t, 2, r.Files)
	assert.Equal(t, 2, r.OK)
	assert.Empty(t, r.Problems)
	assert.Empty(t, r.Fixed)
}

func TestVerifyFindsAndFixes(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	timePath := c.Path(c.Entry(idTime))

	// Moved inside its folder, a stale manifest hash, and an orphan.
	require.NoError(t, os.Rename(timePath, filepath.Join(c.Dir, "moved.m4a")))
	c.Entry(idMoney).AudioSHA256 = ""
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, "stray.m4a"), []byte("not audio"), 0o600))

	r, err := f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ProblemMoved, ProblemHashMismatch, ProblemOrphan}, kinds(r.Problems))
	assert.Empty(t, r.Fixed, "nothing changes without --fix")
	assert.Empty(t, c.Entry(idMoney).AudioSHA256)

	r, err = f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ProblemMoved, ProblemHashMismatch}, kinds(r.Fixed))
	assert.Equal(t, "moved.m4a", c.Entry(idTime).File)
	assert.NotEmpty(t, c.Entry(idMoney).AudioSHA256)
	assert.Equal(t, "moved.m4a", readManifest(t, c.Dir).Tracks[0].File, "fixes are saved")

	r, err = f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{ProblemOrphan}, kinds(r.Problems), "orphans are only reported")
}

func TestVerifyTagAndAudioProblems(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	timePath := c.Path(c.Entry(idTime))
	moneyPath := c.Path(c.Entry(idMoney))

	require.NoError(t, mp4tag.Write(timePath, mp4tag.Tags{Freeform: map[string]string{TagSpotifyID: "someoneElse"}}))
	// Swap Money's audio for other audio with mixtape's tags intact.
	other := f.wav(880)
	m4a := filepath.Join(t.TempDir(), "other.m4a")
	require.NoError(t, AFConvert{}.Encode(t.Context(), other, m4a))
	tags := f.tags(moneyPath)
	data, err := os.ReadFile(m4a)
	require.NoError(t, err)
	retagged, err := mp4tag.Tag(data, tags)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(moneyPath, retagged, 0o600))

	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ProblemTagMismatch, ProblemHashMismatch}, kinds(r.Problems))
	assert.Equal(t, []string{ProblemHashMismatch}, kinds(r.Fixed))
	assert.Equal(t, StatusPending, c.Entry(idMoney).Status, "changed audio is recorded again")
	assert.Equal(t, StatusRecorded, c.Entry(idTime).Status, "a tag mismatch is only reported")
}

func TestVerifyGuardsAgainstUnmountedLibrary(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	for _, e := range c.Manifest.Tracks {
		require.NoError(t, os.Remove(c.Path(&e)))
	}

	r, err := f.lib.Verify(t.Context(), true, false)
	require.ErrorIs(t, err, ErrUnsafeFix)
	assert.Equal(t, []string{ProblemMissing, ProblemMissing}, kinds(r.Problems))
	assert.Equal(t, StatusRecorded, c.Entry(idTime).Status, "nothing was changed")

	r, err = f.lib.Verify(t.Context(), true, true)
	require.NoError(t, err)
	assert.Len(t, r.Fixed, 2)
	assert.Equal(t, StatusPending, c.Entry(idTime).Status, "--force marks them for re-recording")
	assert.Empty(t, c.Entry(idTime).File)

	require.NoError(t, os.RemoveAll(f.root))
	_, err = f.lib.Verify(t.Context(), true, false)
	require.ErrorIs(t, err, ErrUnsafeFix)
	r, err = f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{ProblemMissing}, kinds(r.Problems))
}

func TestVerifyStatusMismatch(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	c.Entry(idTime).Status = StatusFailed

	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{ProblemStatusMismatch}, kinds(r.Fixed))
	assert.Equal(t, StatusRecorded, c.Entry(idTime).Status)
}

func TestAFConvert(t *testing.T) {
	t.Parallel()
	requireAAC(t)
	f := newFixture(t, nil)
	out := filepath.Join(t.TempDir(), "out.m4a")
	require.NoError(t, AFConvert{}.Encode(t.Context(), f.wav(440), out))
	hash, err := mp4tag.HashFile(out)
	require.NoError(t, err)
	assert.Len(t, hash, 64)

	bad := filepath.Join(t.TempDir(), "bad.wav")
	require.NoError(t, os.WriteFile(bad, []byte("not a wav"), 0o600))
	err = AFConvert{Bitrate: 128000}.Encode(t.Context(), bad, filepath.Join(t.TempDir(), "x.m4a"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "afconvert")
}

func TestArtwork(t *testing.T) {
	t.Parallel()
	requireAAC(t)
	jpeg := []byte{0xFF, 0xD8, 0xFF, 0xE0, 'c', 'o', 'v', 'e', 'r'}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cover.jpg":
			_, _ = w.Write(jpeg)
		case "/gif":
			_, _ = w.Write([]byte("GIF89a"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	f := newFixture(t, nil)
	f.lib.opts.HTTPClient = srv.Client()
	c := f.collection(Spec{Kind: KindSingles})

	cases := map[string]struct {
		url   string
		cover []byte
	}{
		"downloaded":   {srv.URL + "/cover.jpg", jpeg},
		"http error":   {srv.URL + "/missing", nil},
		"not an image": {srv.URL + "/gif", nil},
		"plain http":   {"http://example.com/cover.jpg", nil},
	}
	freq := 300.0
	for name, tc := range cases {
		freq += 50
		tr := Track{ID: "artwork" + name[:4] + "00000000000", Title: name, Artist: "A", ArtworkURL: tc.url}
		e := f.commit(c, tr, freq, CommitOptions{Artwork: true})
		assert.Equal(t, tc.cover, f.tags(filepath.Join(c.Dir, e.File)).Cover, name)
	}
	assert.Len(t, f.warns, 3, "each failed download is a warning, never an error")

	e := f.commit(c, Track{ID: "noArtwork000000000000", Title: "off", Artist: "A", ArtworkURL: srv.URL + "/cover.jpg"},
		999, CommitOptions{})
	assert.Nil(t, f.tags(filepath.Join(c.Dir, e.File)).Cover, "artwork is opt-in")
}
