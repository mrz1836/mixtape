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

func TestWriteJSON(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, WriteJSON(path, map[string]any{"b": 1, "a": []string{"x"}}, 0o600))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.JSONEq(t, `{"a": ["x"], "b": 1}`, string(got))
	assert.Equal(t, "{\n  \"a\": [\n    \"x\"\n  ],\n  \"b\": 1\n}\n", string(got), "indented, one newline at the end") //nolint:testifylint // the layout itself is under test
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.Error(t, WriteJSON(path, func() {}, 0o600), "a value JSON cannot hold")
	require.Error(t, WriteJSON(filepath.Join(t.TempDir(), "missing", "x.json"), 1, 0o600), "the folder must exist")
}

func TestOpenAppend(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "logs", "session.log")
	for _, line := range []string{"one\n", "two\n"} {
		f, err := OpenAppend(path)
		require.NoError(t, err)
		_, err = f.WriteString(line)
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "one\ntwo\n", string(got), "each open appends")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	dir, err := os.Stat(filepath.Dir(path))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dir.Mode().Perm())

	file := filepath.Join(t.TempDir(), "plain")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, err = OpenAppend(filepath.Join(file, "under-a-file.log"))
	require.Error(t, err)
}

func TestWriteAtomicFailures(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	require.ErrorIs(t, WriteAtomic("", nil, 0o600), ErrEmptyPath)
	require.Error(t, WriteAtomic(filepath.Join(dir, "missing", "x"), nil, 0o600), "no folder to hold the temp file")

	// The target is a folder with something in it: the rename fails, and
	// the temp file is removed rather than left behind.
	target := filepath.Join(dir, "taken")
	require.NoError(t, os.MkdirAll(filepath.Join(target, "inside"), 0o700))
	require.Error(t, WriteAtomic(target, []byte("x"), 0o600))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, IsTemp(e.Name()), "leftover %s", e.Name())
	}

	require.Error(t, CopyFile(filepath.Join(dir, "no-such-source"), filepath.Join(dir, "dst"), 0o600))
	// A folder opens, but cannot be read as a file: the copy fails while
	// filling the temp file, and nothing lands at dst.
	err = CopyFile(target, filepath.Join(dir, "dst"), 0o600)
	require.ErrorContains(t, err, "writing temp file")
	assert.NoFileExists(t, filepath.Join(dir, "dst"))
}
