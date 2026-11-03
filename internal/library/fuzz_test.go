package library

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// FuzzLoadManifest feeds arbitrary bytes to the manifest reader. Every
// collection folder's mixtape.json goes through it on Open, so it must
// never panic, must reject only with ErrBadManifest, and whatever it
// accepts must survive what Save does next: sort the tracks, render the
// playlist, and encode a manifest that reads back the same.
func FuzzLoadManifest(f *testing.F) {
	f.Add(writtenManifest(f))
	f.Add([]byte(`{`))
	f.Add([]byte(`{"schema":99}`))
	f.Add([]byte(`null`))
	f.Add([]byte(`{"schema":1,"tracks":[{"position":-3,"status":"removed","recorded_at":"1999-12-31T23:59:59+14:00"}]}`))
	// Inputs run one at a time in each fuzzing process, so they share one
	// folder: a fresh temp dir per input costs more than the input itself.
	dir := filepath.Join(f.TempDir(), "Fuzz")
	require.NoError(f, os.Mkdir(dir, 0o750))
	path := filepath.Join(dir, ManifestName)

	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(path, data, 0o600))
		m, err := loadManifest(path)
		if err != nil {
			require.ErrorIs(t, err, ErrBadManifest)
			return
		}
		require.LessOrEqual(t, m.Schema, Schema)

		// Save's steps without its fsyncs, which would cap the fuzzer at a
		// few hundred inputs a second.
		c := &Collection{Dir: dir, Manifest: m}
		c.sortTracks()
		_ = c.playlist()
		out, err := json.MarshalIndent(&c.Manifest, "", "  ")
		if err != nil {
			return // a time JSON cannot hold; Save reports it as an error
		}
		require.NoError(t, os.WriteFile(path, out, 0o600))
		again, err := loadManifest(path)
		require.NoError(t, err, "what mixtape writes, mixtape reads")
		require.Len(t, again.Tracks, len(c.Manifest.Tracks))
		for i := range again.Tracks {
			want, got := c.Manifest.Tracks[i], again.Tracks[i]
			require.Equal(t, want.RecordedAt == nil, got.RecordedAt == nil)
			if want.RecordedAt != nil {
				// The same instant; the zone pointer need not be the same.
				require.True(t, want.RecordedAt.Equal(*got.RecordedAt))
			}
			want.RecordedAt, got.RecordedAt = nil, nil
			require.Equal(t, want, got)
		}
	})
}

// writtenManifest returns a mixtape.json as the library writes it, with a
// relinked partial recording, an encode failure with a kept WAV and a
// removed track, so the fuzzer starts from every field a real one has.
func writtenManifest(f *testing.F) []byte {
	f.Helper()
	now := time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)
	lib, err := Open(f.TempDir(), Options{
		Encoder: &testutil.Encoder{}, WorkDir: f.TempDir(), Now: func() time.Time { return now },
	})
	require.NoError(f, err)
	c, err := lib.Collection(albumSpec())
	require.NoError(f, err)
	relinked := trackTime()
	relinked.PlayedID, relinked.ReleaseDate = idRick, "1973-03-01"
	rick := Track{ID: idRick, Title: "Never Gonna Give You Up", Artist: "Rick Astley"}
	c.Sync([]Track{relinked, trackMoney(), rick}, true)
	_, err = lib.Commit(f.Context(), c, relinked, testutil.SineWAV(f, 440), CommitOptions{Partial: true, RecordedMS: 412000})
	require.NoError(f, err)
	money := c.Entry(idMoney)
	money.Status, money.FailReason, money.PendingWAV = StatusEncodeFailed, "afconvert: boom", "/tmp/failed/Money.wav"
	c.Sync([]Track{relinked, trackMoney()}, true)
	require.NoError(f, lib.Save(c))

	data, err := os.ReadFile(filepath.Join(c.Dir, ManifestName))
	require.NoError(f, err)
	return data
}
