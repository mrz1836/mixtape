package fileutil

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWriteAtomic(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")

	require.NoError(t, WriteAtomic(path, []byte("one"), 0o600))
	require.NoError(t, WriteAtomic(path, []byte("two"), 0o600))

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "two", string(got))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	assertNoTemps(t, dir)
}

func TestWriteAtomicErrors(t *testing.T) {
	t.Parallel()
	require.ErrorIs(t, WriteAtomic("", nil, 0o600), ErrEmptyPath)

	missing := filepath.Join(t.TempDir(), "no", "such", "dir", "f")
	require.Error(t, WriteAtomic(missing, []byte("x"), 0o600))
}

func TestCopyFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.bin")
	dst := filepath.Join(dir, "sub", "dst.bin")
	require.NoError(t, os.WriteFile(src, []byte("audio"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Dir(dst), 0o750))

	require.NoError(t, CopyFile(src, dst, 0o644))

	got, err := os.ReadFile(dst)
	require.NoError(t, err)
	assert.Equal(t, "audio", string(got))
	info, err := os.Stat(dst)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o644), info.Mode().Perm())
	assertNoTemps(t, filepath.Dir(dst))

	require.Error(t, CopyFile(filepath.Join(dir, "missing"), dst, 0o644))
}

func TestIsTemp(t *testing.T) {
	t.Parallel()
	assert.True(t, IsTemp(TempPrefix+"123"))
	assert.False(t, IsTemp("song.m4a"))
}

func assertNoTemps(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, IsTemp(e.Name()), "leftover temp file %s", e.Name())
	}
}
