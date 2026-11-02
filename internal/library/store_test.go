package library

import (
	"context"
	"encoding/binary"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// untaggableEncoder writes a file whose audio hashes but whose moov holds a
// child that overruns it, so tagging fails after a good encode.
type untaggableEncoder struct{}

func (untaggableEncoder) Encode(_ context.Context, _, m4aPath string) error {
	bad := make([]byte, 18)
	binary.BigEndian.PutUint32(bad, 100) // claims 100 bytes in an 18-byte moov
	copy(bad[4:], "mvhd")
	out := append(mp4box("ftyp", []byte("M4A \x00\x00\x02\x00")), mp4box("moov", bad)...)
	out = append(out, mp4box("mdat", []byte("audio"))...)
	return os.WriteFile(m4aPath, out, 0o600)
}

func mp4box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

func TestTagFailureKeepsTheWAVForARetry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, untaggableEncoder{})
	c := f.collection(playlistSpec())
	wavPath := f.wav(440)

	e, err := f.lib.Commit(t.Context(), c, trackTime(), wavPath, CommitOptions{})
	require.ErrorIs(t, err, ErrEncode, "the worker reports the song failed and keeps it pending")
	assert.Equal(t, StatusEncodeFailed, e.Status)
	assert.FileExists(t, e.PendingWAV, "the recording is not lost")
	assert.Contains(t, e.FailReason, "tag:")
	assert.Equal(t, StatusEncodeFailed, readManifest(t, c.Dir).Tracks[0].Status)
}

func TestCheckOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.ErrorIs(t, checkOutput(filepath.Join(dir, "missing.m4a")), fs.ErrNotExist,
		"the reason the file is missing is kept")

	empty := filepath.Join(dir, "empty.m4a")
	require.NoError(t, os.WriteFile(empty, nil, 0o600))
	require.ErrorIs(t, checkOutput(empty), errEmptyOutput)

	full := filepath.Join(dir, "full.m4a")
	require.NoError(t, os.WriteFile(full, []byte("audio"), 0o600))
	require.NoError(t, checkOutput(full))
}
