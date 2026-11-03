package mp4tag

import (
	"bytes"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// tagFileBytes tags the file at path with TagFile and returns what WriteTo
// writes.
func tagFileBytes(t *testing.T, path string, tags Tags) []byte {
	t.Helper()
	r, err := TagFile(path, tags)
	require.NoError(t, err)
	defer func() { require.NoError(t, r.Close()) }()
	var b bytes.Buffer
	n, err := r.WriteTo(&b)
	require.NoError(t, err)
	assert.Equal(t, int64(b.Len()), n, "WriteTo counts what it wrote")
	assert.Equal(t, r.Size(), n, "Size is the length written")
	return b.Bytes()
}

// TagFile is Tag for a file on disk: for every layout testutil.M4A writes,
// and tags that grow, keep or empty moov, the stream is the bytes Tag
// returns.
func TestTagFileMatchesTag(t *testing.T) {
	t.Parallel()
	tagSets := map[string]Tags{
		"full":  fullTags(),
		"empty": {},
		"same":  {Title: "An old title, long enough that dropping it shrinks moov"},
	}
	for _, v := range variants() {
		for name, tags := range tagSets {
			t.Run(v.name+", "+name, func(t *testing.T) {
				t.Parallel()
				file := testutil.M4A(v.opts)
				want, wantErr := Tag(file, tags)
				path := testutil.WriteFile(t, "song.m4a", file)
				if wantErr != nil {
					_, err := TagFile(path, tags)
					require.Error(t, err)
					return
				}
				assert.Equal(t, want, tagFileBytes(t, path, tags))
			})
		}
	}
}

// TagFile refuses what Tag refuses, with the same error, before anything
// is written.
func TestTagFileRejectsWhatTagRejects(t *testing.T) {
	t.Parallel()
	bad := malformedInputs()
	for _, name := range slices.Sorted(maps.Keys(bad)) {
		tc := bad[name]
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := TagFile(testutil.WriteFile(t, "bad.m4a", tc.data), fullTags())
			require.ErrorIs(t, err, tc.err)
		})
	}
	t.Run("cover format", func(t *testing.T) {
		t.Parallel()
		tags := fullTags()
		tags.Cover = []byte("GIF89a")
		_, err := TagFile(testutil.WriteFile(t, "song.m4a", testutil.M4A(testutil.M4AOptions{})), tags)
		require.ErrorIs(t, err, ErrCoverFormat)
	})
	t.Run("missing file", func(t *testing.T) {
		t.Parallel()
		_, err := TagFile(filepath.Join(t.TempDir(), "missing.m4a"), Tags{})
		require.ErrorIs(t, err, os.ErrNotExist)
	})
	t.Run("a folder", func(t *testing.T) {
		t.Parallel()
		_, err := TagFile(t.TempDir(), Tags{})
		require.Error(t, err)
	})
	t.Run("moov past the size guard", func(t *testing.T) {
		t.Parallel()
		path := testutil.WriteFile(t, "big.m4a", []byte{0x04, 0x00, 0x00, 0x01, 'm', 'o', 'o', 'v'}) // 64 MiB + 1
		require.NoError(t, os.Truncate(path, maxMoovSize+1))                                         // sparse zeros
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		require.NoError(t, err)
		_, err = f.Write(testutil.Box("mdat", []byte("audio")))
		require.NoError(t, err)
		require.NoError(t, f.Close())
		_, err = TagFile(path, Tags{})
		require.ErrorIs(t, err, errTooLarge)
	})
}

var errSink = errors.New("disk full")

// failingWriter accepts n bytes, then fails.
type failingWriter struct{ n int }

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) <= w.n {
		w.n -= len(p)
		return len(p), nil
	}
	k := w.n
	w.n = 0
	return k, errSink
}

func TestTagFileWriteErrors(t *testing.T) {
	t.Parallel()
	file := testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte("audio"), 1000)})
	path := testutil.WriteFile(t, "song.m4a", file)

	for _, after := range []int{0, 10, 2000} {
		r, err := TagFile(path, fullTags())
		require.NoError(t, err)
		_, err = r.WriteTo(&failingWriter{n: after})
		require.ErrorIs(t, err, errSink, "a failing writer after %d bytes", after)
		require.NoError(t, r.Close())
	}

	// The audio shrinks between TagFile and WriteTo: the copy comes up
	// short instead of writing a truncated file without complaint.
	r, err := TagFile(path, fullTags())
	require.NoError(t, err)
	defer func() { _ = r.Close() }()
	require.NoError(t, os.Truncate(path, int64(len(file)-100)))
	_, err = r.WriteTo(io.Discard)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
}

// allocatedBy is how many bytes fn allocates. Only a test that does not
// run in parallel with others can rely on it.
func allocatedBy(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// The audio is copied, never held: TagFile reads moov, and WriteTo streams
// the rest.
func TestTagFileDoesNotLoadTheAudio(t *testing.T) { //nolint:paralleltest // measures allocations, so it runs alone
	path := testutil.WriteFile(t, "long.m4a", testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0xA5}, 16<<20)}))
	allocated := allocatedBy(func() {
		r, err := TagFile(path, fullTags())
		require.NoError(t, err)
		_, err = r.WriteTo(io.Discard)
		require.NoError(t, err)
		require.NoError(t, r.Close())
	})
	assert.Less(t, allocated, uint64(4<<20), "a 16 MiB song is tagged without loading it")
}

// FuzzTagFile checks TagFile against Tag on arbitrary bytes: both refuse
// the same inputs, and what they accept they write the same.
func FuzzTagFile(f *testing.F) {
	addSeeds(f)
	path := filepath.Join(f.TempDir(), "fuzz.m4a")
	f.Fuzz(func(t *testing.T, data []byte) {
		tags := fullTags()
		want, wantErr := Tag(data, tags)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		r, err := TagFile(path, tags)
		if wantErr != nil {
			require.Error(t, err, "TagFile accepted what Tag refused: %v", wantErr)
			return
		}
		require.NoError(t, err, "TagFile refused what Tag accepted")
		defer func() { _ = r.Close() }()
		var b bytes.Buffer
		_, err = r.WriteTo(&b)
		require.NoError(t, err)
		assert.Equal(t, want, b.Bytes())
	})
}
