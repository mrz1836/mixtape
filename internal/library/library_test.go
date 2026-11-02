package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	requireAAC(t)
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)

	wavPath := f.wav(440)
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

	keep := f.wav(330)
	_, err = f.lib.Commit(t.Context(), c, trackMoney(), keep, CommitOptions{KeepWAV: true})
	require.NoError(t, err)
	assert.FileExists(t, keep, "--keep-wav keeps it")
}

func readManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

func TestCommitAlbumNumbers(t *testing.T) {
	t.Parallel()
	requireAAC(t)
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
	requireAAC(t)
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
	requireAAC(t)
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

func TestPartialIsNotDone(t *testing.T) {
	t.Parallel()
	requireAAC(t)
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
	requireAAC(t)
	f := newFixture(t, failEncoder{})
	c := f.collection(playlistSpec())

	wavPath := f.wav(440)
	e, err := f.lib.Commit(t.Context(), c, trackTime(), wavPath, CommitOptions{})
	require.ErrorIs(t, err, ErrEncode)
	require.ErrorIs(t, err, errFakeEncode)
	assert.Equal(t, StatusEncodeFailed, e.Status)
	assert.Equal(t, filepath.Join(f.work, "failed", "Today’s-Top-Hits-"+idTime+".wav"), e.PendingWAV)
	assert.FileExists(t, e.PendingWAV)
	assert.NoFileExists(t, wavPath)
	assert.Contains(t, e.FailReason, "fake encoder failure")
	assert.Equal(t, StatusEncodeFailed, readManifest(t, c.Dir).Tracks[0].Status)

	// The next run (a fresh Open) finishes the job without recording.
	lib := f.open(nil)
	c2, ok := lib.Lookup(playlistSpec())
	require.True(t, ok)
	done, err := lib.RetryEncode(t.Context(), c2, idTime, CommitOptions{})
	require.NoError(t, err)
	assert.Equal(t, StatusRecorded, done.Status)
	assert.Empty(t, done.PendingWAV)
	assert.NoFileExists(t, e.PendingWAV)
	assert.True(t, c2.Done(idTime))

	_, err = lib.RetryEncode(t.Context(), c2, idTime, CommitOptions{})
	require.ErrorIs(t, err, errNoPendingWAV)
	_, err = lib.RetryEncode(t.Context(), c2, idRick, CommitOptions{})
	require.ErrorIs(t, err, errNoEntry)
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
	requireAAC(t)
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
	assert.NotEmpty(t, f.warns)

	_, err = f.open(nil).Lock()
	require.ErrorIs(t, err, ErrLocked)

	unlock()
	unlock2, err := f.open(nil).Lock()
	require.NoError(t, err)
	unlock2()
}

func TestReconcileAfterCrash(t *testing.T) {
	t.Parallel()
	requireAAC(t)
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	timeEntry := f.commit(c, trackTime(), 440, CommitOptions{})
	moneyEntry := f.commit(c, trackMoney(), 550, CommitOptions{})

	// Simulate crashes after the file landed but before the manifest did:
	// one file under its expected name, one renamed by hand.
	require.NoError(t, os.Rename(filepath.Join(c.Dir, moneyEntry.File), filepath.Join(c.Dir, "renamed.m4a")))
	m := readManifest(t, c.Dir)
	for i := range m.Tracks {
		m.Tracks[i].Status, m.Tracks[i].File, m.Tracks[i].AudioSHA256 = StatusPending, "", ""
	}
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, ManifestName), data, 0o600))

	lib := f.open(nil)
	c2, ok := lib.Lookup(playlistSpec())
	require.True(t, ok)
	assert.True(t, c2.Done(idTime))
	assert.Equal(t, timeEntry.AudioSHA256, c2.Entry(idTime).AudioSHA256)
	assert.Equal(t, "renamed.m4a", c2.Entry(idMoney).File)
	assert.Equal(t, StatusPending, readManifest(t, c.Dir).Tracks[0].Status, "Open itself never writes")

	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	assert.Equal(t, StatusRecorded, readManifest(t, c.Dir).Tracks[0].Status, "Lock persists the repair")
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

	lib := f.open(nil)
	require.Len(t, lib.Collections(), 1)
	joined := strings.Join(f.warns, "\n")
	assert.Contains(t, joined, "skipping Broken")
	assert.Contains(t, joined, "schema 99")
}
