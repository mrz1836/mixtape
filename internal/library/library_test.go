package library

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestOpenCreatesRoot(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "a", "b")
	lib, err := Open(root, Options{})
	require.NoError(t, err)
	assert.Equal(t, root, lib.Root())
	assert.Empty(t, lib.Collections())
	assert.DirExists(t, root)
}

func TestCollectionCreateFindRefresh(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)

	_, ok := f.lib.Lookup(playlistSpec())
	assert.False(t, ok)
	assert.NoDirExists(t, filepath.Join(f.root, "Today’s-Top-Hits"), "Lookup never creates")

	c := f.collection(playlistSpec())
	assert.Equal(t, "Today’s-Top-Hits", c.Folder())
	assert.FileExists(t, filepath.Join(c.Dir, ManifestName))
	assert.FileExists(t, filepath.Join(c.Dir, "Today’s-Top-Hits.m3u8"))
	assert.Equal(t, Schema, c.Manifest.Schema)
	assert.Equal(t, f.now, c.Manifest.UpdatedAt)

	found, ok := f.lib.Lookup(playlistSpec())
	require.True(t, ok)
	assert.Same(t, c, found)

	renamed := playlistSpec()
	renamed.Name = "Top Hits 2026"
	again := f.collection(renamed)
	assert.Same(t, c, again, "found by Spotify ID")
	assert.Equal(t, "Top Hits 2026", again.Manifest.Name)
	assert.Equal(t, "Today’s-Top-Hits", again.Folder(), "a renamed playlist keeps its folder")

	singles := f.collection(Spec{Kind: KindSingles})
	assert.Equal(t, "Singles", singles.Folder())
	assert.Equal(t, "Singles", singles.Manifest.Name)
	assert.Same(t, singles, f.collection(Spec{Kind: KindSingles}))

	road := f.collection(Spec{Kind: KindFolder, Name: "Road Trip"})
	assert.Same(t, road, f.collection(Spec{Kind: KindFolder, Name: "road trip"}), "folder names match case-insensitively")

	names := make([]string, 0, 3)
	for _, col := range f.lib.Collections() {
		names = append(names, col.Folder())
	}
	assert.Equal(t, []string{"Road-Trip", "Singles", "Today’s-Top-Hits"}, names)
}

func TestCollectionFolderCollisions(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	other := f.collection(Spec{Kind: KindPlaylist, SpotifyID: "otherPlaylistID000000", Name: "Today’s Top Hits"})
	assert.Equal(t, "Today’s-Top-Hits", other.Folder())

	c := f.collection(playlistSpec())
	assert.Equal(t, "Today’s-Top-Hits-37i9dQ", c.Folder(), "same name, different playlist")

	require.NoError(t, os.Mkdir(filepath.Join(f.root, "Chill"), 0o750))
	chill := f.collection(Spec{Kind: KindPlaylist, SpotifyID: "chillPlaylist00000000", Name: "Chill"})
	assert.Equal(t, "Chill-chillP", chill.Folder(), "a playlist never adopts a stranger's folder")

	require.NoError(t, os.Mkdir(filepath.Join(f.root, "Mine"), 0o750))
	mine := f.collection(Spec{Kind: KindFolder, Name: "Mine"})
	assert.Equal(t, "Mine", mine.Folder(), "--into adopts an existing plain folder")

	folder := f.collection(Spec{Kind: KindFolder, Name: "Chill"})
	assert.Equal(t, "Chill", folder.Folder(), "the plain Chill folder is free for a named folder")
}

func TestCollectionSpecErrors(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	for _, s := range []Spec{
		{Kind: KindPlaylist, Name: "no id"},
		{Kind: KindAlbum, Name: "no id"},
		{Kind: KindFolder, Name: "  "},
		{Kind: "mixtape", Name: "x"},
	} {
		_, err := f.lib.Collection(s)
		require.ErrorIs(t, err, errSpec, "%+v", s)
	}
}

func TestEnsureAndSync(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())

	c.Sync([]Track{trackTime(), trackMoney(), trackTime()}, true)
	require.Len(t, c.Manifest.Tracks, 2, "a duplicate keeps its first position")
	assert.Equal(t, idTime, c.Manifest.Tracks[0].RequestedID)
	assert.Equal(t, 1, c.Manifest.Tracks[0].Position)
	assert.Equal(t, StatusPending, c.Manifest.Tracks[1].Status)

	c.Entry(idTime).Status = StatusFailed
	rick := Track{ID: idRick, Title: "Never Gonna Give You Up", Artist: "Rick Astley"}
	c.Sync([]Track{rick, trackTime()}, true)
	require.Len(t, c.Manifest.Tracks, 3)
	assert.Equal(t, []string{idRick, idTime, idMoney}, ids(c))
	assert.Equal(t, StatusFailed, c.Entry(idTime).Status, "existing tracks keep their status")
	assert.Equal(t, StatusRemoved, c.Entry(idMoney).Status, "a complete list removes what it lacks")
	assert.Equal(t, 3, c.Entry(idMoney).Position)

	money := c.Entry(idMoney)
	money.File, money.AudioSHA256 = "Pink-Floyd-Money.m4a", "abc"
	c.Sync([]Track{trackMoney()}, false)
	assert.Equal(t, StatusRecorded, c.Entry(idMoney).Status, "a returning track gets its recording back")
	assert.Equal(t, StatusPending, c.Entry(idRick).Status, "an incomplete list removes nothing")
	assert.Equal(t, []string{idMoney, idRick, idTime}, ids(c))

	e := c.Ensure(Track{ID: idTime, Title: "ignored", Popularity: 70})
	assert.Equal(t, "Time", e.Title, "Ensure only fills gaps")
	assert.Equal(t, 70, e.Popularity)

	n := c.Ensure(Track{ID: "newTrack0000000000000", Title: "New"})
	assert.Equal(t, 4, n.Position)
	assert.Equal(t, StatusPending, n.Status)
}

func ids(c *Collection) []string {
	out := make([]string, 0, len(c.Manifest.Tracks))
	for _, e := range c.Manifest.Tracks {
		out = append(out, e.RequestedID)
	}
	return out
}

func TestSyncNeverRemovesEverythingForAnEmptyList(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	c.Sync(nil, true)
	for _, e := range c.Manifest.Tracks {
		assert.NotEqual(t, StatusRemoved, e.Status, e.Title)
	}
	c.Sync([]Track{trackTime()}, true)
	assert.Equal(t, StatusRemoved, c.Entry(trackMoney().ID).Status, "a non-empty complete list still removes")
}

func TestSyncMatchesRelinkedIDs(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Ensure(Track{ID: idTime, PlayedID: idRick, Title: "Time"})

	c.Sync([]Track{{ID: idRick, Title: "Time (relinked)"}}, true)
	require.Len(t, c.Manifest.Tracks, 1, "the played ID identifies the same entry")
	assert.Equal(t, idTime, c.Manifest.Tracks[0].RequestedID)
	assert.Equal(t, "Time (relinked)", c.Manifest.Tracks[0].Title)
}

func TestCommitPlaylist(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)

	wavPath := testutil.SineWAV(t, 440)
	e, err := f.lib.Commit(t.Context(), c, trackTime(), wavPath, CommitOptions{RecordedMS: 413100})
	require.NoError(t, err)
	assert.NoFileExists(t, wavPath, "the WAV is removed")

	assert.Equal(t, StatusRecorded, e.Status)
	assert.Equal(t, "Pink-Floyd-Time.m4a", e.File)
	assert.Equal(t, int64(413100), e.RecordedMS)
	require.NotNil(t, e.RecordedAt)
	assert.Equal(t, f.now, *e.RecordedAt)
	assert.True(t, c.Done(idTime))

	path := filepath.Join(c.Dir, e.File)
	tags := f.tags(path)
	assert.Equal(t, "Time", tags.Title)
	assert.Equal(t, "Pink Floyd", tags.Artist)
	assert.Equal(t, "The Dark Side of the Moon", tags.Album)
	assert.Equal(t, "Today’s Top Hits", tags.Grouping)
	assert.Equal(t, "https://open.spotify.com/track/"+idTime, tags.Comment)
	assert.Equal(t, 4, tags.TrackNumber, "playlists keep the album track number")
	assert.Equal(t, idTime, tags.Freeform[TagSpotifyID])
	assert.Empty(t, tags.Freeform[TagRequestedID])
	assert.Equal(t, e.AudioSHA256, tags.Freeform[TagAudioSHA256])

	onDisk := readManifest(t, c.Dir)
	assert.Equal(t, StatusRecorded, onDisk.Tracks[0].Status)
	assert.Equal(t, e.AudioSHA256, onDisk.Tracks[0].AudioSHA256)

	playlist, err := os.ReadFile(filepath.Join(c.Dir, "Today’s-Top-Hits.m3u8"))
	require.NoError(t, err)
	assert.Equal(t, "#EXTM3U\n#EXTINF:413,Pink Floyd - Time\nPink-Floyd-Time.m4a\n", string(playlist))

	locs := f.lib.Find(idTime)
	require.Len(t, locs, 1)
	assert.Equal(t, path, locs[0].Path)
	assert.Len(t, f.lib.FindHash(e.AudioSHA256), 1)
	assert.Empty(t, f.lib.Find(idMoney))
	assert.Empty(t, f.lib.Find(""))
	assert.Empty(t, f.lib.FindHash(""))

	keep := testutil.SineWAV(t, 330)
	_, err = f.lib.Commit(t.Context(), c, trackMoney(), keep, CommitOptions{KeepWAV: true})
	require.NoError(t, err)
	assert.FileExists(t, keep, "--keep-wav keeps it")
}

func TestCommitAlbumNumbers(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(albumSpec())
	assert.Equal(t, "Pink-Floyd-The-Dark-Side-of-the-Moon", c.Folder())
	c.Sync([]Track{trackMoney(), trackTime()}, true)

	e := f.commit(c, trackTime(), 440, CommitOptions{})
	assert.Equal(t, "02-Pink-Floyd-Time.m4a", e.File, "NN is the album position")
	tags := f.tags(filepath.Join(c.Dir, e.File))
	assert.Equal(t, 2, tags.TrackNumber)
	assert.Equal(t, 2, tags.TrackTotal)
}

func TestCommitRelinked(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	tr := trackTime()
	tr.PlayedID = idRick

	e := f.commit(c, tr, 440, CommitOptions{})
	assert.Equal(t, idRick, e.PlayedID)
	tags := f.tags(filepath.Join(c.Dir, e.File))
	assert.Equal(t, idRick, tags.Freeform[TagSpotifyID])
	assert.Equal(t, idTime, tags.Freeform[TagRequestedID])
	assert.Equal(t, "https://open.spotify.com/track/"+idRick, tags.Comment)
	assert.Len(t, f.lib.Find(idRick), 1)
	assert.Len(t, f.lib.Find(idTime), 1)
}

func TestCommitNameCollisionAndRedo(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(Spec{Kind: KindSingles})

	first := f.commit(c, Track{ID: idTime, Title: "Intro", Artist: "Band"}, 440, CommitOptions{})
	second := f.commit(c, Track{ID: idMoney, Title: "Intro", Artist: "Band"}, 550, CommitOptions{})
	assert.Equal(t, "Band-Intro.m4a", first.File)
	assert.Equal(t, "Band-Intro-0vFOza.m4a", second.File, "a different track gets the ID suffix")

	redo := f.commit(c, Track{ID: idTime, Title: "Intro", Artist: "Band"}, 660, CommitOptions{})
	assert.Equal(t, first.File, redo.File, "a redo overwrites in place")
	assert.NotEqual(t, first.AudioSHA256, redo.AudioSHA256)
	entries, err := os.ReadDir(c.Dir)
	require.NoError(t, err)
	assert.Len(t, entries, 4, "two songs, a manifest and a playlist")
}

func TestCommitLooksUpTheReleaseDate(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	asked := []string{}
	f.lib.opts.ReleaseDate = func(_ context.Context, id string) (string, error) {
		asked = append(asked, id)
		return "1973-03-01", nil
	}
	c := f.collection(playlistSpec())
	e := f.commit(c, trackTime(), 440, CommitOptions{})
	assert.Equal(t, "1973-03-01", e.ReleaseDate)
	assert.Equal(t, "1973-03-01", f.tags(c.Path(&e)).Date, "the file is tagged with the year")
	assert.Equal(t, []string{trackTime().ID}, asked)

	dated := trackMoney()
	dated.ReleaseDate = "1979-11-30"
	e = f.commit(c, dated, 550, CommitOptions{})
	assert.Equal(t, "1979-11-30", e.ReleaseDate, "a known date is kept")
	assert.Len(t, asked, 1, "and not looked up")
}

func TestPartialNeverReplacesACompleteRecording(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(Spec{Kind: KindSingles})
	good := f.commit(c, trackTime(), 440, CommitOptions{})

	wav := testutil.SineWAV(t, 550)
	got, err := f.lib.Commit(t.Context(), c, trackTime(), wav, CommitOptions{Partial: true})
	require.ErrorIs(t, err, ErrKeptComplete)
	assert.Equal(t, good.AudioSHA256, got.AudioSHA256, "the complete entry is reported back unchanged")
	assert.Equal(t, good.AudioSHA256, c.Entry(trackTime().ID).AudioSHA256)
	assert.False(t, c.Entry(trackTime().ID).Partial)
	hash, err := mp4tag.HashFile(c.Path(c.Entry(trackTime().ID)))
	require.NoError(t, err)
	assert.Equal(t, good.AudioSHA256, hash, "the file on disk is the complete recording")
	assert.NoFileExists(t, wav, "the partial's WAV is not left behind")
}

func TestCommitIntoAnUnwritableFolderIsAStoreError(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	f := newFixture(t, nil)
	c := f.collection(Spec{Kind: KindSingles})
	require.NoError(t, f.lib.Save(c))
	require.NoError(t, os.Chmod(c.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })

	wav := testutil.SineWAV(t, 440)
	_, err := f.lib.Commit(t.Context(), c, trackTime(), wav, CommitOptions{})
	require.ErrorIs(t, err, ErrStore)
	assert.NoFileExists(t, wav, "the WAV is not leaked")
}

func TestPartialIsNotDone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())

	partial := f.commit(c, trackTime(), 440, CommitOptions{Partial: true})
	assert.Equal(t, StatusRecorded, partial.Status)
	assert.True(t, partial.Partial)
	assert.False(t, c.Done(idTime), "a kept partial is re-recorded next run")
	assert.Empty(t, f.lib.Find(idTime), "and is never copied elsewhere")
	assert.Empty(t, f.lib.FindHash(partial.AudioSHA256))
	assert.Equal(t, "1", f.tags(filepath.Join(c.Dir, partial.File)).Freeform[TagPartial])

	full := f.commit(c, trackTime(), 550, CommitOptions{})
	assert.Equal(t, partial.File, full.File, "the full recording replaces it in place")
	assert.False(t, full.Partial)
	assert.True(t, c.Done(idTime))
	assert.Empty(t, f.tags(filepath.Join(c.Dir, full.File)).Freeform[TagPartial])
}

func TestEncodeFailureKeepsWAVAndRetries(t *testing.T) {
	t.Parallel()
	enc := &testutil.Encoder{}
	enc.Fail(errFakeEncode)
	f := newFixture(t, enc)
	c := f.collection(playlistSpec())

	wavPath := testutil.SineWAV(t, 440)
	e, err := f.lib.Commit(t.Context(), c, trackTime(), wavPath, CommitOptions{})
	require.ErrorIs(t, err, ErrEncode)
	require.ErrorIs(t, err, errFakeEncode)
	assert.Equal(t, StatusEncodeFailed, e.Status)
	assert.Equal(t, filepath.Join(f.work, "failed", "Today’s-Top-Hits-"+idTime+".wav"), e.PendingWAV)
	assert.FileExists(t, e.PendingWAV)
	assert.NoFileExists(t, wavPath)
	assert.Contains(t, e.FailReason, "fake encoder failure")
	assert.Equal(t, StatusEncodeFailed, readManifest(t, c.Dir).Tracks[0].Status)

	// The next run (a fresh Open) tries again; failing again keeps the WAV
	// where it already is.
	lib, c2 := f.reopen(playlistSpec())
	again, err := lib.RetryEncode(t.Context(), c2, idTime, CommitOptions{})
	require.ErrorIs(t, err, ErrEncode)
	assert.Equal(t, e.PendingWAV, again.PendingWAV)
	assert.FileExists(t, again.PendingWAV)

	// Once encoding works, the job finishes without recording.
	enc.Fail(nil)
	done, err := lib.RetryEncode(t.Context(), c2, idTime, CommitOptions{})
	require.NoError(t, err)
	assert.Equal(t, StatusRecorded, done.Status)
	assert.Empty(t, done.PendingWAV)
	assert.Empty(t, done.FailReason)
	assert.NoFileExists(t, e.PendingWAV)
	assert.True(t, c2.Done(idTime))
	assert.Equal(t, 3, enc.Runs(), "one encode per attempt")

	_, err = lib.RetryEncode(t.Context(), c2, idTime, CommitOptions{})
	require.ErrorIs(t, err, errNoPendingWAV)
	_, err = lib.RetryEncode(t.Context(), c2, idRick, CommitOptions{})
	require.ErrorIs(t, err, errNoEntry)
}

func TestRetryEncodeNeedsTheKeptWAV(t *testing.T) {
	t.Parallel()
	enc := &testutil.Encoder{}
	enc.Fail(errFakeEncode)
	f := newFixture(t, enc)
	c := f.collection(playlistSpec())
	e, err := f.lib.Commit(t.Context(), c, trackTime(), testutil.SineWAV(t, 440), CommitOptions{})
	require.ErrorIs(t, err, ErrEncode)

	require.NoError(t, os.Remove(e.PendingWAV)) // cleaned up by hand, or a wiped temp folder
	enc.Fail(nil)
	_, err = f.lib.RetryEncode(t.Context(), c, idTime, CommitOptions{})
	require.ErrorIs(t, err, errNoPendingWAV)
	require.ErrorIs(t, err, fs.ErrNotExist)
	assert.Equal(t, 1, enc.Runs(), "nothing is encoded without the WAV")
}

func TestCommitRejectsTrackWithoutID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	_, err := f.lib.Commit(t.Context(), c, Track{Title: "x"}, "missing.wav", CommitOptions{})
	require.ErrorIs(t, err, errSpec)
	_, err = f.lib.Copy(t.Context(), c, Track{Title: "x"}, Location{})
	require.ErrorIs(t, err, errSpec)
}

func TestCopyRetagsAndKeepsHash(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	singles := f.collection(Spec{Kind: KindSingles})
	src := f.commit(singles, trackTime(), 440, CommitOptions{RecordedMS: 413000})

	album := f.collection(albumSpec())
	album.Sync([]Track{trackMoney(), {ID: idTime, Title: "Time", Artist: "Pink Floyd"}}, true)
	locs := f.lib.Find(idTime)
	require.Len(t, locs, 1)

	e, err := f.lib.Copy(t.Context(), album, Track{ID: idTime, Title: "Time", Artist: "Pink Floyd"}, locs[0])
	require.NoError(t, err)
	assert.Equal(t, StatusCopied, e.Status)
	assert.Equal(t, "02-Pink-Floyd-Time.m4a", e.File)
	assert.Equal(t, src.AudioSHA256, e.AudioSHA256, "copying never changes the audio")
	assert.Equal(t, filepath.Join("Singles", "Pink-Floyd-Time.m4a"), e.CopiedFrom)
	assert.Equal(t, int64(413000), e.RecordedMS)
	assert.Equal(t, "The Dark Side of the Moon", e.Album, "metadata gaps are filled from the source")
	assert.True(t, album.Done(idTime))

	tags := f.tags(filepath.Join(album.Dir, e.File))
	assert.Equal(t, "The Dark Side of the Moon", tags.Grouping)
	assert.Equal(t, 2, tags.TrackNumber)
	assert.Equal(t, src.AudioSHA256, tags.Freeform[TagAudioSHA256])
	assert.Equal(t, "Singles", f.tags(locs[0].Path).Grouping, "the source is untouched")
	assert.Len(t, f.lib.FindHash(src.AudioSHA256), 2)

	_, err = f.lib.Copy(t.Context(), album, trackMoney(), Location{Path: filepath.Join(f.root, "nope.m4a")})
	require.Error(t, err)
}

func TestMark(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)

	require.NoError(t, f.lib.Mark(c, idTime, StatusUnavailable, "did not start"))
	got := readManifest(t, c.Dir).Tracks[0]
	assert.Equal(t, StatusUnavailable, got.Status)
	assert.Equal(t, "did not start", got.FailReason)
	require.ErrorIs(t, f.lib.Mark(c, idRick, StatusFailed, ""), errNoEntry)
}

func TestLock(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	stale := filepath.Join(c.Dir, ".mixtape-tmp-123")
	require.NoError(t, os.WriteFile(stale, []byte("x"), 0o600))

	unlock, err := f.lib.Lock()
	require.NoError(t, err)
	assert.NoFileExists(t, stale, "taking the lock removes temp files of a crashed write")
	assert.NotEmpty(t, f.warnings())

	_, err = f.open().Lock()
	require.ErrorIs(t, err, ErrLocked)

	unlock()
	unlock2, err := f.open().Lock()
	require.NoError(t, err)
	unlock2()
}

// TestLockKeepsTheWAVWhenTheRepairCannotBeSaved: a kept WAV is removed
// only once the manifest that points at it is rewritten.
func TestLockKeepsTheWAVWhenTheRepairCannotBeSaved(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)
	f.commit(c, trackTime(), 440, CommitOptions{})
	kept := testutil.WriteFile(t, "kept.wav", []byte("RIFF"))
	crash(t, c.Dir, func(m *Manifest) {
		forget(&m.Tracks[0])
		m.Tracks[0].Status, m.Tracks[0].PendingWAV = StatusEncodeFailed, kept
	})
	lib, _ := f.reopen(playlistSpec())
	require.NoError(t, os.Chmod(c.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })

	_, err := lib.Lock()
	require.ErrorContains(t, err, "save manifest")
	assert.FileExists(t, kept, "the manifest on disk still needs it")

	require.NoError(t, os.Chmod(c.Dir, 0o700))
	unlock, err := lib.Lock()
	require.NoError(t, err, "the failed Lock released the lock")
	unlock()
	assert.Equal(t, StatusRecorded, readManifest(t, c.Dir).Tracks[0].Status)
	assert.NoFileExists(t, kept)
}

func TestUnreadableManifestIsSkipped(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	f.collection(playlistSpec())
	bad := filepath.Join(f.root, "Broken")
	require.NoError(t, os.Mkdir(bad, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(bad, ManifestName), []byte("{"), 0o600))
	future := filepath.Join(f.root, "Future")
	require.NoError(t, os.Mkdir(future, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(future, ManifestName), []byte(`{"schema":99}`), 0o600))

	lib := f.open()
	require.Len(t, lib.Collections(), 1)
	joined := strings.Join(f.warnings(), "\n")
	assert.Contains(t, joined, "skipping Broken")
	assert.Contains(t, joined, "schema 99")
}
