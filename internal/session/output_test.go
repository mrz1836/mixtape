package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
)

func TestOutputRestoreRecord(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	lock := filepath.Join(dir, "mixtape", "recorder.lock")
	path := OutputRestorePath(lock)
	assert.Equal(t, filepath.Join(dir, "mixtape", "output-restore.json"), path, "next to the recorder lock")

	got, err := ReadOutputRestore(path)
	require.NoError(t, err, "no file is no record")
	assert.False(t, got.Pending())

	r := capture.OutputRestore{
		DeviceUID: "BuiltInSpeakerDevice", DeviceName: "MacBook Pro Speakers", Original: 48000, Set: 44100,
		SessionID: "s1", At: time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC),
	}
	require.NoError(t, WriteOutputRestore(path, r))
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	got, err = ReadOutputRestore(path)
	require.NoError(t, err)
	assert.Equal(t, r, got)

	require.NoError(t, WriteOutputRestore(path, capture.OutputRestore{}))
	assert.NoFileExists(t, path, "nothing pending: no file")
	require.NoError(t, WriteOutputRestore(path, capture.OutputRestore{}), "clearing twice is fine")

	require.NoError(t, os.WriteFile(path, []byte("{"), 0o600))
	_, err = ReadOutputRestore(path)
	require.Error(t, err)
	_, err = ReadOutputRestore(dir)
	require.Error(t, err, "a folder is not a record")

	file := filepath.Join(dir, "plain")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	require.Error(t, WriteOutputRestore(filepath.Join(file, "x.json"), r), "no folder to write into")
}
