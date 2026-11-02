package library

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// verifyFixture is a playlist of two recorded songs.
func verifyFixture(t *testing.T) (*fixture, *Collection) {
	t.Helper()
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
	swapAudio(t, moneyPath)

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
	assert.Contains(t, r.Problems[0].Detail, f.root)
	r, err = f.lib.Verify(t.Context(), true, true)
	require.NoError(t, err, "--force on a missing folder only reports it")
	assert.Empty(t, r.Fixed)
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

func TestArtwork(t *testing.T) {
	t.Parallel()
	jpeg := testutil.JPEG()
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
	assert.Len(t, f.warnings(), 3, "each failed download is a warning, never an error")

	e := f.commit(c, Track{ID: "noArtwork000000000000", Title: "off", Artist: "A", ArtworkURL: srv.URL + "/cover.jpg"},
		999, CommitOptions{})
	assert.Nil(t, f.tags(filepath.Join(c.Dir, e.File)).Cover, "artwork is opt-in")
}

func TestVerifySafetyGateCountsOnlyLostSongs(t *testing.T) {
	t.Parallel()
	t.Run("songs found under other names", func(t *testing.T) {
		t.Parallel()
		f, c := verifyFixture(t)
		for _, e := range c.Manifest.Tracks {
			require.NoError(t, os.Rename(c.Path(&e), filepath.Join(c.Dir, "renamed-"+e.File)))
		}
		r, err := f.lib.Verify(t.Context(), true, false)
		require.NoError(t, err, "a drive whose songs were all found is mounted")
		assert.Equal(t, []string{ProblemMoved, ProblemMoved}, kinds(r.Fixed))
		assert.Equal(t, "renamed-Pink-Floyd-Time.m4a", c.Entry(idTime).File)
		assert.True(t, c.Done(idTime))
	})
	t.Run("half the songs missing", func(t *testing.T) {
		t.Parallel()
		f, c := verifyFixture(t)
		require.NoError(t, os.Remove(c.Path(c.Entry(idTime))))
		r, err := f.lib.Verify(t.Context(), true, false)
		require.NoError(t, err, "only more than half looks unmounted")
		assert.Equal(t, []string{ProblemMissing}, kinds(r.Fixed))
		e := c.Entry(idTime)
		assert.Equal(t, StatusPending, e.Status)
		assert.Empty(t, e.File)
		assert.Empty(t, e.AudioSHA256)
		assert.Equal(t, "file missing (verify)", e.FailReason)
		assert.True(t, c.Done(idMoney))
	})
}

func TestVerifyReportsSongsMovedOutOfTheirFolder(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	singles := f.collection(Spec{Kind: KindSingles})
	require.NoError(t, os.Rename(c.Path(c.Entry(idTime)), filepath.Join(singles.Dir, "Time.m4a")))
	require.NoError(t, os.Rename(c.Path(c.Entry(idMoney)), filepath.Join(f.root, "Money.m4a")))

	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Equal(t, 2, r.Collections)
	require.Equal(t, []string{ProblemMoved, ProblemMoved}, kinds(r.Problems), "found by hash, so not orphans")
	assert.Equal(t, "found at Money.m4a (outside its folder; move it back to fix)", r.Problems[0].Detail)
	assert.Equal(t, "found at "+filepath.Join("Singles", "Time.m4a")+" (outside its folder; move it back to fix)",
		r.Problems[1].Detail)
	assert.Empty(t, r.Fixed, "moving a file back is left to the owner")
	assert.Equal(t, "Pink-Floyd-Time.m4a", c.Entry(idTime).File)
	assert.Equal(t, StatusRecorded, c.Entry(idTime).Status)
}

func TestVerifyUnreadableSongIsRecordedAgain(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	require.NoError(t, os.WriteFile(c.Path(c.Entry(idTime)), []byte("not an mp4"), 0o600))

	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Equal(t, []string{ProblemUnreadable}, kinds(r.Fixed))
	assert.Equal(t, 1, r.OK)
	assert.Equal(t, StatusPending, c.Entry(idTime).Status)
	assert.Equal(t, "unreadable file (verify)", c.Entry(idTime).FailReason)
}

// TestSongsVerifySendsBackStayPending: the file of a song verify --fix
// sends back for changed or truncated audio still names its entry, but
// must not be taken back, by the next Open or by a second --fix.
func TestSongsVerifySendsBackStayPending(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	swapAudio(t, c.Path(c.Entry(idMoney)))
	truncate(t, c.Path(c.Entry(idTime)))
	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{ProblemHashMismatch, ProblemUnreadable}, kinds(r.Fixed))

	lib, c2 := f.reopen(playlistSpec())
	assert.Equal(t, StatusPending, c2.Entry(idMoney).Status)
	assert.Equal(t, StatusPending, c2.Entry(idTime).Status)
	r, err = lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Empty(t, r.Problems)
	assert.Empty(t, r.Fixed)
	assert.Equal(t, StatusPending, c2.Entry(idMoney).Status)

	// Recording it again settles it, in place.
	again, err := lib.Commit(t.Context(), c2, trackMoney(), testutil.SineWAV(t, 660), CommitOptions{})
	require.NoError(t, err)
	assert.Equal(t, "Pink-Floyd-Money.m4a", again.File)
	r, err = lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.OK)
}

// TestVerifyRefreshesAStaleManifestHash: a redo replaced the file but its
// manifest write was lost, so the manifest still holds the old take's hash.
func TestVerifyRefreshesAStaleManifestHash(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	oldManifest, err := os.ReadFile(filepath.Join(c.Dir, ManifestName))
	require.NoError(t, err)
	redo := f.commit(c, trackTime(), 660, CommitOptions{})
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, ManifestName), oldManifest, 0o600))

	lib, c2 := f.reopen(playlistSpec())
	require.NotEqual(t, redo.AudioSHA256, c2.Entry(idTime).AudioSHA256)
	r, err := lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	require.Len(t, r.Fixed, 1)
	assert.Equal(t, ProblemHashMismatch, r.Fixed[0].Kind)
	assert.Equal(t, "manifest hash is stale", r.Fixed[0].Detail)
	assert.Equal(t, redo.AudioSHA256, c2.Entry(idTime).AudioSHA256)
	assert.Equal(t, StatusRecorded, c2.Entry(idTime).Status)
}

func TestVerifyScansOnlyCollectionAudio(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	// Time's file went to the trash: hidden folders are never searched.
	trash := filepath.Join(f.root, ".Trashes")
	require.NoError(t, os.Mkdir(trash, 0o750))
	require.NoError(t, os.Rename(c.Path(c.Entry(idTime)), filepath.Join(trash, "Time.m4a")))
	writeSong(t, c.Dir, "EXTRA.M4A", "an orphan, whatever its case", nil)
	writeSong(t, c.Dir, ".tagging-1.m4a", "a crashed retag", nil)
	writeSong(t, f.root, "loose.m4a", "outside every collection", nil)
	other := filepath.Join(f.root, "Not a collection")
	require.NoError(t, os.Mkdir(other, 0o750))
	writeSong(t, other, "song.m4a", "in a folder without a manifest", nil)
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, "cover.jpg"), testutil.JPEG(), 0o600))

	r, err := f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	require.Equal(t, []string{ProblemOrphan, ProblemMissing}, kinds(r.Problems))
	assert.Equal(t, "EXTRA.M4A", r.Problems[0].File)
	assert.Equal(t, "Pink-Floyd-Time.m4a", r.Problems[1].File)
}

func TestVerifyChecksOnlyDoneSongs(t *testing.T) {
	t.Parallel()
	f, c := verifyFixture(t)
	rick := Track{ID: idRick, Title: "Never Gonna Give You Up", Artist: "Rick Astley"}
	c.Sync([]Track{trackTime(), rick}, true) // Money left the playlist; its file stays

	r, err := f.lib.Verify(t.Context(), true, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.Files)
	assert.Equal(t, 1, r.OK)
	assert.Empty(t, r.Problems, "a removed song's file is neither checked nor an orphan")
	assert.Equal(t, StatusRemoved, c.Entry(idMoney).Status)
	assert.Equal(t, StatusPending, c.Entry(idRick).Status)
}

func TestVerifyStopsWhenCanceled(t *testing.T) {
	t.Parallel()
	f, _ := verifyFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := f.lib.Verify(ctx, false, false)
	require.ErrorIs(t, err, context.Canceled)
}

func TestVerifySkipsAFolderItCannotRead(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads any folder")
	}
	f, c := verifyFixture(t)
	require.NoError(t, os.Chmod(c.Dir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })

	r, err := f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, []string{ProblemMissing, ProblemMissing}, kinds(r.Problems))
	_, err = f.lib.Verify(t.Context(), true, false)
	require.ErrorIs(t, err, ErrUnsafeFix, "the safety gate keeps --fix from resetting them")
}

func TestVerifyFixReportsAManifestItCannotSave(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	f, c := verifyFixture(t)
	require.NoError(t, os.Chmod(c.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })

	_, err := f.lib.Verify(t.Context(), true, false)
	require.ErrorContains(t, err, "save manifest")
}
