package mp4tag

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// addSeeds seeds f with every testutil.M4A layout (see variants), one file
// as Tag writes it, and the malformed inputs Tag must refuse. The fuzzer
// minimizes every input that finds new code, at a cost that grows with
// the square of its size, and fuzzes less while it does; a tiny audio
// payload keeps the seeds, and so that pause, small.
func addSeeds(f *testing.F) {
	f.Helper()
	for _, v := range variants() {
		o := v.opts
		o.Audio, o.Chunks = []byte("tiny audio chunk"), 2
		f.Add(testutil.M4A(o))
	}
	tagged, err := Tag(testutil.M4A(testutil.M4AOptions{Audio: []byte("audio"), Chunks: 1}), fullTags())
	require.NoError(f, err)
	f.Add(tagged)
	bad := malformedInputs()
	for _, name := range slices.Sorted(maps.Keys(bad)) {
		f.Add(bad[name].data)
	}
}

// writtenIlst reads the direct children of file's ilst, the one Tag
// rebuilds, the way Parse reads them. Unlike Parse it sees no tag atoms
// elsewhere in moov, which Tag copies without looking at.
func writtenIlst(t *testing.T, file []byte) Tags {
	t.Helper()
	moov, err := child(file, "moov") // the first top-level moov, as Tag and Parse find it
	require.NoError(t, err)
	require.NotNil(t, moov)
	udta, err := child(moov.payload(), "udta")
	require.NoError(t, err)
	require.NotNil(t, udta, "Tag always writes a udta")
	meta, err := child(udta.payload(), "meta")
	require.NoError(t, err)
	require.NotNil(t, meta, "Tag always writes a meta")
	children, err := metaPayload(*meta)
	require.NoError(t, err)
	ilst, err := child(children, "ilst")
	require.NoError(t, err)
	require.NotNil(t, ilst, "Tag always writes an ilst")

	var got Tags
	require.NoError(t, walkChildren(ilst.payload(), func(b box) error {
		if a, ok := findTagAtom(b.typ); ok {
			return readTagAtom(&got, a, b)
		}
		if ff, ok := parseFreeform(b); ok && string(b.typ[:]) == "----" && ff.mean == FreeformMean && ff.hasValue {
			if got.Freeform == nil {
				got.Freeform = map[string]string{}
			}
			got.Freeform[ff.name] = ff.value
		}
		return nil
	}))
	return got
}

// assertWritten checks that got holds every value of want, whose fields
// are all set: the managed fields (the cover only when cover is true) and
// the freeform names want lists. Any other freeform atom in got is one Tag
// kept.
func assertWritten(t *testing.T, want, got Tags, cover bool) {
	t.Helper()
	for name, val := range want.Freeform {
		assert.Equal(t, val, got.Freeform[name], "freeform %q", name)
	}
	want.Freeform, got.Freeform = nil, nil
	if !cover {
		want.Cover, got.Cover = nil, nil
	}
	assert.Equal(t, want, got)
}

// FuzzParse feeds Parse arbitrary bytes: it may refuse them, never panic.
func FuzzParse(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(_ *testing.T, data []byte) {
		_, _ = Parse(data)
	})
}

// FuzzReadFile reads arbitrary bytes from a file. ReadFile and HashFile
// must not panic, and where the in-memory Parse and AudioSHA256 also
// accept the bytes, the file readers must agree with them.
func FuzzReadFile(f *testing.F) {
	addSeeds(f)
	// Each fuzzing process runs inputs one at a time, so one file per
	// process, rewritten for every input, is enough. A fresh temp folder
	// per input costs milliseconds and starves the fuzzer.
	path := filepath.Join(f.TempDir(), "fuzz.m4a")
	f.Fuzz(func(t *testing.T, data []byte) {
		require.NoError(t, os.WriteFile(path, data, 0o600))
		tags, readErr := ReadFile(path)
		hash, hashErr := HashFile(path)
		if want, err := AudioSHA256(data); err == nil && hashErr == nil {
			assert.Equal(t, want, hash)
		}
		if want, err := Parse(data); err == nil && readErr == nil {
			assert.Equal(t, want, tags)
		}
	})
}

// FuzzTag tags arbitrary bytes. Tag may refuse them, never panic; what it
// accepts must come out with the same audio, with the tags in the ilst it
// rebuilt, and unchanged when tagged again with the same tags.
func FuzzTag(f *testing.F) {
	addSeeds(f)
	f.Fuzz(func(t *testing.T, data []byte) {
		tags := fullTags()
		out, err := Tag(data, tags)
		if err != nil {
			return
		}

		before, err := AudioSHA256(data)
		require.NoError(t, err, "Tag accepted input whose audio cannot be hashed")
		after, err := AudioSHA256(out)
		require.NoError(t, err)
		assert.Equal(t, before, after, "tagging changed the audio")

		again, err := Tag(out, tags)
		require.NoError(t, err)
		assert.Equal(t, out, again, "tagging the tagged file again changed it")

		assertWritten(t, tags, writtenIlst(t, out), true)

		// Parse walks boxes Tag copies without reading (inside a track, when
		// the audio does not move), so only input Parse accepts has to
		// parse once tagged. Parse keeps the last text, number and freeform
		// value it meets, and Tag writes those last in moov, so they are
		// the ones written. It keeps the first cover, though, which can be
		// one Tag copied from outside the ilst; writtenIlst checked the
		// cover instead.
		if _, parseErr := Parse(data); parseErr != nil {
			return
		}
		got, err := Parse(out)
		require.NoError(t, err)
		assertWritten(t, tags, got, false)
	})
}
