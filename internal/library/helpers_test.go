package library

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// Public track IDs used across the tests.
const (
	idTime  = "3TO7bbrUKrOSPGRTB5MeCz" // Pink Floyd - Time
	idMoney = "0vFOzaXqZHahrZp6enQwQb" // Pink Floyd - Money
	idRick  = "4cOdK2wGLETKBW3PvgPWqT" // Rick Astley - Never Gonna Give You Up
	idAlbum = "4LH4d3cOWNNsVw41Gqt2kv" // The Dark Side of the Moon
	idList  = "37i9dQZF1DXcBWIGoYBM5M" // Today's Top Hits
)

var errFakeEncode = errors.New("fake encoder failure")

// fixture is a library in a temp dir with captured warnings.
type fixture struct {
	t   *testing.T
	lib *Library
	enc Encoder // every Open of the fixture encodes with it
	// codec and policy are the Options.Codec and CopyPolicy of every Open.
	codec  mp4tag.Codec
	policy CopyPolicy
	root   string
	work   string
	now    time.Time
	mu     sync.Mutex
	warns  []string
}

// newFixture opens a library in a temp dir that encodes with enc. Unlike
// Options, nil means a testutil.Encoder, so the suite runs where afconvert
// cannot encode AAC (Linux CI, sandboxes); pass AFConvert{} for the real
// encoder.
func newFixture(t *testing.T, enc Encoder) *fixture {
	t.Helper()
	if enc == nil {
		enc = &testutil.Encoder{}
	}
	f := &fixture{t: t, enc: enc, root: t.TempDir(), work: t.TempDir(), now: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)}
	f.lib = f.open()
	return f
}

// newCodecFixture opens a library that records codec with a testutil.Encoder
// and copies under policy.
func newCodecFixture(t *testing.T, codec mp4tag.Codec, policy CopyPolicy) *fixture {
	t.Helper()
	f := newFixture(t, &testutil.Encoder{Codec: string(codec)})
	f.codec, f.policy = codec, policy
	f.lib = f.open()
	return f
}

// as opens the library again as a run that records codec under policy
// would: a codec change between two runs.
func (f *fixture) as(codec mp4tag.Codec, policy CopyPolicy) *Library {
	f.t.Helper()
	f.enc, f.codec, f.policy = &testutil.Encoder{Codec: string(codec)}, codec, policy
	f.lib = f.open()
	return f.lib
}

// open opens the library again, as the next run of mixtape would.
func (f *fixture) open() *Library {
	f.t.Helper()
	lib, err := Open(f.root, Options{
		Encoder:    f.enc,
		Codec:      f.codec,
		CopyPolicy: f.policy,
		WorkDir:    f.work,
		Now:        func() time.Time { return f.now },
		Warn: func(msg string) {
			f.mu.Lock()
			f.warns = append(f.warns, msg)
			f.mu.Unlock()
		},
	})
	require.NoError(f.t, err)
	return lib
}

// warnings returns the warnings reported so far.
func (f *fixture) warnings() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.warns...)
}

// commit records t into c from a fresh sine WAV at freq; distinct
// frequencies give distinct audio hashes.
func (f *fixture) commit(c *Collection, t Track, freq float64, o CommitOptions) Entry {
	f.t.Helper()
	e, err := f.lib.Commit(f.t.Context(), c, t, testutil.SineWAV(f.t, freq), o)
	require.NoError(f.t, err)
	return e
}

func (f *fixture) collection(s Spec) *Collection {
	f.t.Helper()
	c, err := f.lib.Collection(s)
	require.NoError(f.t, err)
	return c
}

func (f *fixture) tags(path string) mp4tag.Tags {
	f.t.Helper()
	tags, err := mp4tag.ReadFile(path)
	require.NoError(f.t, err)
	return tags
}

func playlistSpec() Spec {
	return Spec{
		Kind: KindPlaylist, SpotifyID: idList, Name: "Today’s Top Hits", Owner: "Spotify",
		URL: "https://open.spotify.com/playlist/" + idList,
	}
}

func albumSpec() Spec {
	return Spec{Kind: KindAlbum, SpotifyID: idAlbum, Name: "The Dark Side of the Moon", Artist: "Pink Floyd"}
}

func trackTime() Track {
	return Track{
		ID: idTime, Title: "Time", Artist: "Pink Floyd", Album: "The Dark Side of the Moon",
		AlbumArtist: "Pink Floyd", DurationMS: 413000, TrackNumber: 4, DiscNumber: 1,
	}
}

func trackMoney() Track {
	return Track{ID: idMoney, Title: "Money", Artist: "Pink Floyd", DurationMS: 382000, TrackNumber: 6}
}

func readManifest(t *testing.T, dir string) Manifest {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ManifestName))
	require.NoError(t, err)
	var m Manifest
	require.NoError(t, json.Unmarshal(data, &m))
	return m
}

// crash rewrites the manifest in dir as edit leaves it: the state a crash
// between a file's rename into place and the manifest write leaves behind.
func crash(t *testing.T, dir string, edit func(m *Manifest)) {
	t.Helper()
	m := readManifest(t, dir)
	edit(&m)
	data, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ManifestName), data, 0o600))
}

// forget turns e back into the pending entry it was before its recording
// was stored, relink included.
func forget(e *Entry) {
	e.Status, e.File, e.AudioSHA256, e.PlayedID = StatusPending, "", "", ""
	e.Partial, e.RecordedAt, e.RecordedMS = false, nil, 0
}

// swapAudio replaces the audio of the song at path but keeps its tags, so
// its MIXTAPE_AUDIO_SHA256 no longer matches.
func swapAudio(t *testing.T, path string) {
	t.Helper()
	tags, err := mp4tag.ReadFile(path)
	require.NoError(t, err)
	data, err := mp4tag.Tag(testutil.M4A(testutil.M4AOptions{Audio: []byte("other audio")}), tags)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// retag replaces the tags of the song at path with tags; its audio stays.
func retag(t *testing.T, path string, tags mp4tag.Tags) {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	data, err = mp4tag.Tag(data, tags)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o600))
}

// truncate cuts the end off the song at path: its tags (moov comes first)
// stay readable, its audio does not.
func truncate(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Truncate(path, info.Size()-4))
}

// writeSong writes a synthetic M4A holding audio and tagged with freeform
// as dir/name, and returns its path.
func writeSong(t *testing.T, dir, name, audio string, freeform map[string]string) string {
	t.Helper()
	data, err := mp4tag.Tag(testutil.M4A(testutil.M4AOptions{Audio: []byte(audio)}), mp4tag.Tags{Freeform: freeform})
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, data, 0o600))
	return path
}

// reopen opens the library again, as the next run would, and returns it
// with its copy of the collection s names.
func (f *fixture) reopen(s Spec) (*Library, *Collection) {
	f.t.Helper()
	lib := f.open()
	c, ok := lib.Lookup(s)
	require.True(f.t, ok)
	return lib, c
}
