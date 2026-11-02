package library

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
)

func TestReconcileAfterCrash(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	timeEntry := f.commit(c, trackTime(), 440, CommitOptions{})
	moneyEntry := f.commit(c, trackMoney(), 550, CommitOptions{})

	// Simulate crashes after the file landed but before the manifest did:
	// one file under its expected name, one renamed by hand. Time had been
	// kept as a WAV for a re-encode before it was stored.
	require.NoError(t, os.Rename(c.Path(&moneyEntry), filepath.Join(c.Dir, "renamed.m4a")))
	kept := filepath.Join(f.work, "failed", "kept.wav")
	require.NoError(t, os.MkdirAll(filepath.Dir(kept), 0o750))
	require.NoError(t, os.WriteFile(kept, []byte("RIFF"), 0o600))
	crash(t, c.Dir, func(m *Manifest) {
		forget(&m.Tracks[0])
		forget(&m.Tracks[1])
		m.Tracks[0].Status, m.Tracks[0].PendingWAV, m.Tracks[0].FailReason = StatusEncodeFailed, kept, "afconvert: boom"
	})

	lib, c2 := f.reopen(playlistSpec())
	got := c2.Entry(idTime)
	assert.True(t, c2.Done(idTime))
	assert.Equal(t, timeEntry.AudioSHA256, got.AudioSHA256, "the hash comes from the file")
	assert.Empty(t, got.PendingWAV)
	assert.Empty(t, got.FailReason)
	assert.Equal(t, "renamed.m4a", c2.Entry(idMoney).File)
	assert.Equal(t, moneyEntry.AudioSHA256, c2.Entry(idMoney).AudioSHA256)
	assert.Equal(t, StatusEncodeFailed, readManifest(t, c.Dir).Tracks[0].Status, "Open itself never writes")
	assert.FileExists(t, kept, "a read-only Open deletes nothing")

	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	assert.Equal(t, StatusRecorded, readManifest(t, c.Dir).Tracks[0].Status, "Lock persists the repair")
	assert.NoFileExists(t, kept, "the writer removes the WAV the repair made unnecessary")
	playlist, err := os.ReadFile(filepath.Join(c.Dir, c.Folder()+m3u8Ext))
	require.NoError(t, err)
	assert.Contains(t, string(playlist), "renamed.m4a")
}

func TestReconcileAdoptsAFileUnderTheEntrysOwnName(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(albumSpec())
	c.Sync([]Track{trackMoney(), trackTime()}, true)
	timeEntry := f.commit(c, trackTime(), 440, CommitOptions{})
	money := f.commit(c, trackMoney(), 550, CommitOptions{})
	require.NoError(t, os.Rename(c.Path(&money), filepath.Join(c.Dir, "Money-first-take.m4a")))
	crash(t, c.Dir, func(m *Manifest) {
		// A redo of Money failed; its entry still names the earlier take.
		m.Tracks[0].Status, m.Tracks[0].File, m.Tracks[0].FailReason = StatusFailed, "Money-first-take.m4a", "did not start"
		// Time's manifest write was lost: it has no name yet, but the file
		// is under the one its album position gives it.
		forget(&m.Tracks[1])
	})

	_, c2 := f.reopen(albumSpec())
	got := c2.Entry(idTime)
	assert.Equal(t, StatusRecorded, got.Status)
	assert.Equal(t, "02-Pink-Floyd-Time.m4a", got.File)
	assert.Equal(t, timeEntry.AudioSHA256, got.AudioSHA256)
	got = c2.Entry(idMoney)
	assert.True(t, c2.Done(idMoney), "the take the entry names is kept")
	assert.Equal(t, "Money-first-take.m4a", got.File)
	assert.Empty(t, got.FailReason)
	assert.Contains(t, strings.Join(f.warnings(), "\n"), "found 02-Pink-Floyd-Time.m4a on disk")
}

func TestReconcileAdoptsARelinkedRecordingByItsRequestedID(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)
	relinked := trackTime()
	relinked.PlayedID = idRick
	e := f.commit(c, relinked, 440, CommitOptions{})
	require.NoError(t, os.Rename(c.Path(&e), filepath.Join(c.Dir, "Time (single).m4a")))
	crash(t, c.Dir, func(m *Manifest) { forget(&m.Tracks[0]) }) // the relink was lost with the write

	_, c2 := f.reopen(playlistSpec())
	got := c2.Entry(idTime)
	require.NotNil(t, got)
	assert.True(t, c2.Done(idTime), "SPOTIFY_REQUESTED_ID names the entry")
	assert.Equal(t, "Time (single).m4a", got.File)
	assert.Equal(t, e.AudioSHA256, got.AudioSHA256)
	assert.Equal(t, idRick, got.PlayedID, "SPOTIFY_ID restores the relink")
}

func TestReconcileHashesAFileWithoutAHashTag(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)
	require.NoError(t, f.lib.Save(c))
	// Another tagger (or an older mixtape) left only the identity tag.
	path := writeSong(t, c.Dir, "Pink-Floyd-Time.m4a", "time audio", map[string]string{TagSpotifyID: idTime})
	want, err := mp4tag.HashFile(path)
	require.NoError(t, err)

	_, c2 := f.reopen(playlistSpec())
	assert.True(t, c2.Done(idTime))
	assert.Equal(t, want, c2.Entry(idTime).AudioSHA256)
}

func TestReconcileCarriesThePartialFlag(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)
	e := f.commit(c, trackTime(), 440, CommitOptions{Partial: true})
	crash(t, c.Dir, func(m *Manifest) { forget(&m.Tracks[0]) })

	lib, c2 := f.reopen(playlistSpec())
	got := c2.Entry(idTime)
	assert.Equal(t, StatusRecorded, got.Status)
	assert.Equal(t, e.File, got.File)
	assert.True(t, got.Partial, "MIXTAPE_PARTIAL=1 marks a kept partial")
	assert.False(t, c2.Done(idTime), "so the next run records it again")
	assert.Empty(t, lib.Find(idTime))
}

func TestReconcileNeverAdoptsDamagedAudio(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	timeEntry := f.commit(c, trackTime(), 440, CommitOptions{})
	money := f.commit(c, trackMoney(), 550, CommitOptions{})

	// Time's audio was replaced under its tags. Money's file lost its end,
	// but a good copy of it sits next to it.
	swapAudio(t, c.Path(&timeEntry))
	good, err := os.ReadFile(c.Path(&money))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, "Money-backup.m4a"), good, 0o600))
	truncate(t, c.Path(&money))
	crash(t, c.Dir, func(m *Manifest) {
		forget(&m.Tracks[0])
		forget(&m.Tracks[1])
	})

	_, c2 := f.reopen(playlistSpec())
	got := c2.Entry(idTime)
	assert.Equal(t, StatusPending, got.Status, "audio that disagrees with its hash tag is recorded again")
	assert.Empty(t, got.File)
	got = c2.Entry(idMoney)
	assert.Equal(t, "Money-backup.m4a", got.File, "a truncated file does not hide a good copy")
	assert.Equal(t, money.AudioSHA256, got.AudioSHA256)
}

func TestReconcileLeavesOtherFilesAlone(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	rick := Track{ID: idRick, Title: "Never Gonna Give You Up", Artist: "Rick Astley"}
	c.Sync([]Track{trackTime(), rick, trackMoney()}, true)
	// Spotify played Time under Rick's ID (one recording listed twice), so
	// Time's file names Rick too; Money left the playlist once recorded.
	relinked := trackTime()
	relinked.PlayedID = idRick
	f.commit(c, relinked, 440, CommitOptions{})
	f.commit(c, trackMoney(), 550, CommitOptions{})
	c.Sync([]Track{trackTime(), rick}, true)
	require.NoError(t, f.lib.Save(c))

	for name, audio := range map[string]string{
		"Rick.aac":         "not an .m4a name",
		".tagging-123.m4a": "a retag that crashed",
	} {
		writeSong(t, c.Dir, name, audio, map[string]string{TagSpotifyID: idRick})
	}
	writeSong(t, c.Dir, "Rick-Astley-Never-Gonna-Give-You-Up.m4a", "Rick's name, another song",
		map[string]string{TagSpotifyID: "1otherTrackID000000000"})
	writeSong(t, c.Dir, "untagged.m4a", "no tags", nil)
	require.NoError(t, os.WriteFile(filepath.Join(c.Dir, "garbage.m4a"), []byte("not an mp4"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(c.Dir, "Rick.m4a"), 0o750))
	before := readManifest(t, c.Dir)
	f.now = f.now.Add(time.Hour) // a rewrite would show in updated_at

	lib, c2 := f.reopen(playlistSpec())
	require.Len(t, c2.Manifest.Tracks, 3)
	got := c2.Manifest.Tracks[1]
	require.Equal(t, idRick, got.RequestedID)
	assert.Equal(t, StatusPending, got.Status, "no file names Rick but the one Time owns")
	assert.Empty(t, got.File)
	assert.Equal(t, StatusRemoved, c2.Entry(idMoney).Status, "a removed song's file does not bring it back")
	assert.NotContains(t, strings.Join(f.warnings(), "\n"), "found")

	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	assert.Equal(t, before.UpdatedAt, readManifest(t, c.Dir).UpdatedAt, "nothing to repair, nothing written")
	assert.NoFileExists(t, filepath.Join(c.Dir, ".tagging-123.m4a"), "the writer removes a crashed retag")
	assert.FileExists(t, filepath.Join(c.Dir, "untagged.m4a"))
}

func TestOpenWithNothingToRepairWritesNothing(t *testing.T) {
	t.Parallel()
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	f.commit(c, trackTime(), 440, CommitOptions{})
	before := readManifest(t, c.Dir)
	f.now = f.now.Add(time.Hour)

	unlock, err := f.open().Lock()
	require.NoError(t, err)
	defer unlock()
	assert.Equal(t, before.UpdatedAt, readManifest(t, c.Dir).UpdatedAt)
	assert.Empty(t, f.warnings())
}

func TestReconcileOfAFolderThatCannotBeListed(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root lists any folder")
	}
	f := newFixture(t, nil)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney()}, true)
	f.commit(c, trackTime(), 440, CommitOptions{})
	money := f.commit(c, trackMoney(), 550, CommitOptions{})
	require.NoError(t, os.Rename(c.Path(&money), filepath.Join(c.Dir, "renamed.m4a")))
	crash(t, c.Dir, func(m *Manifest) {
		forget(&m.Tracks[0])
		forget(&m.Tracks[1])
	})
	require.NoError(t, os.Chmod(c.Dir, 0o300)) // write and search, but no listing
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })

	lib, c2 := f.reopen(playlistSpec())
	assert.True(t, c2.Done(idTime), "a song under its own name is found without a listing")
	assert.False(t, c2.Done(idMoney), "a renamed one needs the listing")
	unlock, err := lib.Lock()
	require.NoError(t, err, "nor does saving the repair")
	unlock()

	require.NoError(t, os.Chmod(c.Dir, 0o700))
	m := readManifest(t, c.Dir)
	assert.Equal(t, StatusRecorded, m.Tracks[0].Status)
	assert.Equal(t, StatusPending, m.Tracks[1].Status)
}

func TestTaggedFor(t *testing.T) {
	t.Parallel()
	plain := Entry{RequestedID: idTime}
	relinked := Entry{RequestedID: idTime, PlayedID: idRick}
	cases := []struct {
		name  string
		tags  map[string]string
		entry Entry
		want  bool
	}{
		{"its own ID", map[string]string{TagSpotifyID: idTime}, plain, true},
		{"the ID it played", map[string]string{TagSpotifyID: idRick}, relinked, true},
		{"requested under another ID", map[string]string{TagSpotifyID: idRick, TagRequestedID: idTime}, plain, true},
		{"another track", map[string]string{TagSpotifyID: idMoney}, relinked, false},
		{"requested as the played ID", map[string]string{TagSpotifyID: idMoney, TagRequestedID: idRick}, relinked, false},
		{"no identity tags", map[string]string{TagAudioSHA256: "abc"}, plain, false},
		{"no tags at all", nil, plain, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, taggedFor(mp4tag.Tags{Freeform: tc.tags}, &tc.entry))
		})
	}
}
