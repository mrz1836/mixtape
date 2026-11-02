package library

import (
	"context"
	"encoding/binary"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// untaggableEncoder writes a file whose audio hashes but whose moov holds a
// child that overruns it, so tagging fails after a good encode.
type untaggableEncoder struct{}

func (untaggableEncoder) Encode(_ context.Context, _, m4aPath string) error {
	bad := make([]byte, 18)
	binary.BigEndian.PutUint32(bad, 100) // claims 100 bytes in an 18-byte moov
	copy(bad[4:], "mvhd")
	out := slices.Concat(testutil.Box("ftyp", []byte("M4A \x00\x00\x02\x00")), testutil.Box("moov", bad),
		testutil.Box("mdat", []byte("audio")))
	return os.WriteFile(m4aPath, out, 0o600)
}

func TestTagFailureKeepsTheWAVForARetry(t *testing.T) {
	t.Parallel()
	f := newFixture(t, untaggableEncoder{})
	c := f.collection(playlistSpec())
	wavPath := testutil.SineWAV(t, 440)

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

// TestAFConvert runs the real encoder: macOS only, outside a sandbox.
func TestAFConvert(t *testing.T) {
	t.Parallel()
	testutil.RequireAAC(t)
	out := filepath.Join(t.TempDir(), "out.m4a")
	require.NoError(t, AFConvert{}.Encode(t.Context(), testutil.SineWAV(t, 440), out))
	hash, err := mp4tag.HashFile(out)
	require.NoError(t, err)
	assert.Len(t, hash, 64)

	bad := testutil.WriteFile(t, "bad.wav", []byte("not a wav"))
	err = AFConvert{Bitrate: 128000}.Encode(t.Context(), bad, filepath.Join(t.TempDir(), "x.m4a"))
	require.ErrorContains(t, err, "afconvert")
}

// TestCommitWithAFConvert stores a real AAC recording end to end: tagging
// must leave afconvert's audio, and so its hash, untouched.
func TestCommitWithAFConvert(t *testing.T) {
	t.Parallel()
	testutil.RequireAAC(t)
	f := newFixture(t, AFConvert{})
	c := f.collection(playlistSpec())
	e := f.commit(c, trackTime(), 440, CommitOptions{})

	path := c.Path(&e)
	hash, err := mp4tag.HashFile(path)
	require.NoError(t, err)
	assert.Equal(t, e.AudioSHA256, hash)
	tags := f.tags(path)
	assert.Equal(t, idTime, tags.Freeform[TagSpotifyID])
	assert.Equal(t, hash, tags.Freeform[TagAudioSHA256])
	assert.Equal(t, "Time", tags.Title)

	r, err := f.lib.Verify(t.Context(), false, false)
	require.NoError(t, err)
	assert.Equal(t, 1, r.OK)
	assert.Empty(t, r.Problems)
}

func TestRetryIntoAnUnwritableFolderKeepsTheRecording(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root writes into read-only folders")
	}
	enc := &testutil.Encoder{}
	enc.Fail(errEncoderDown)
	f := newFixture(t, enc)
	c := f.collection(playlistSpec())
	e, err := f.lib.Commit(t.Context(), c, trackTime(), testutil.SineWAV(t, 440), CommitOptions{})
	require.ErrorIs(t, err, ErrEncode)
	kept := e.PendingWAV
	require.FileExists(t, kept)

	// The encoder works again, but the drive refuses the file.
	enc.Fail(nil)
	require.NoError(t, os.Chmod(c.Dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(c.Dir, 0o700) })
	_, err = f.lib.RetryEncode(t.Context(), c, idTime, CommitOptions{})
	require.ErrorIs(t, err, ErrStore)
	assert.FileExists(t, kept, "the only copy of the recording stays for the next retry")
	assert.Equal(t, kept, c.Entry(idTime).PendingWAV)

	require.NoError(t, os.Chmod(c.Dir, 0o700))
	done, err := f.lib.RetryEncode(t.Context(), c, idTime, CommitOptions{})
	require.NoError(t, err)
	assert.Equal(t, StatusRecorded, done.Status)
	assert.NoFileExists(t, kept, "a stored recording leaves nothing behind")
}

var errEncoderDown = errors.New("encoder unavailable")
