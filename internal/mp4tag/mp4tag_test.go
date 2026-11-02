package mp4tag

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/wav"
)

// requireAAC skips the test unless afconvert can encode AAC here. The AAC
// encoder is invisible inside a sandbox, where afconvert reports "aac" as
// an unknown format.
func requireAAC(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	// afconvert -hf exits non-zero even on success, so only the listing counts.
	out, _ := exec.CommandContext(t.Context(), "afconvert", "-hf").CombinedOutput()
	if !strings.Contains(string(out), "'aac '") {
		t.Skip("afconvert cannot encode AAC here (sandboxed?)")
	}
}

// makeM4A converts a short silent WAV into an M4A with afconvert, the same
// path the app uses at runtime.
func makeM4A(t *testing.T) []byte {
	t.Helper()
	requireAAC(t)
	dir := t.TempDir()
	wavPath := filepath.Join(dir, "in.wav")
	f, err := os.Create(wavPath)
	require.NoError(t, err)
	data := make([]byte, 44100) // ~0.125s stereo 16-bit silence
	require.NoError(t, wav.Write(f, data, 44100, 2))
	require.NoError(t, f.Close())

	m4a := filepath.Join(dir, "in.m4a")
	cmd := exec.CommandContext(t.Context(), "afconvert", wavPath, m4a, "-f", "m4af", "-d", "aac")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "afconvert: %s", out)
	b, err := os.ReadFile(m4a)
	require.NoError(t, err)
	return b
}

// encoderTags drops the iTunSMPB atom (gapless delay/padding) afconvert
// writes into every file, so assertions see only what a test set. Its
// survival through retagging is itself checked in TestEncoderAtomsSurvive.
func encoderTags(tags Tags) Tags {
	delete(tags.Freeform, gaplessAtom)
	if len(tags.Freeform) == 0 {
		tags.Freeform = nil
	}
	return tags
}

const gaplessAtom = "iTunSMPB"

// jpeg is the smallest byte string with JPEG magic; mp4tag only sniffs it.
func jpeg() []byte { return []byte{0xFF, 0xD8, 0xFF, 0xE0, 'j', 'p', 'g'} }

func TestTagRoundTrip(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)

	want := Tags{
		Title:       "Still Loving You",
		Artist:      "Scorpions",
		AlbumArtist: "Scorpions",
		Album:       "Love at First Sting",
		Grouping:    "Late Night",
		Comment:     "https://open.spotify.com/track/abc123",
		TrackNumber: 7,
		TrackTotal:  9,
		DiscNumber:  1,
		DiscTotal:   1,
		Date:        "1984-03-27",
		Freeform:    map[string]string{"SPOTIFY_ID": "abc123", "MIXTAPE_AUDIO_SHA256": "ff00"},
		Cover:       jpeg(),
	}
	out, err := Tag(m4a, want)
	require.NoError(t, err)
	require.NotEqual(t, m4a, out, "output unchanged after tagging")

	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, want, encoderTags(got))
}

func TestEncoderAtomsSurvive(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)
	orig, err := Parse(m4a)
	require.NoError(t, err)
	require.Contains(t, orig.Freeform, gaplessAtom, "afconvert writes gapless info")

	out, err := Tag(m4a, Tags{Title: "T", Freeform: map[string]string{"SPOTIFY_ID": "x"}})
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, orig.Freeform[gaplessAtom], got.Freeform[gaplessAtom])
}

func TestTagEmpty(t *testing.T) {
	t.Parallel()
	out, err := Tag(makeM4A(t), Tags{})
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, Tags{}, encoderTags(got))
}

func TestTagOverwriteExisting(t *testing.T) {
	t.Parallel()
	first, err := Tag(makeM4A(t), Tags{Title: "First", Artist: "A", Cover: jpeg()})
	require.NoError(t, err)
	second, err := Tag(first, Tags{Title: "Second", Artist: "B", Album: "C"})
	require.NoError(t, err)

	got, err := Parse(second)
	require.NoError(t, err)
	assert.Equal(t, Tags{Title: "Second", Artist: "B", Album: "C"}, encoderTags(got),
		"managed atoms, cover included, are replaced")
}

func TestFreeformPreservesUnlisted(t *testing.T) {
	t.Parallel()
	tagged, err := Tag(makeM4A(t), Tags{Freeform: map[string]string{"A": "1", "B": "2"}})
	require.NoError(t, err)

	tagged, err = Tag(tagged, Tags{Title: "T", Freeform: map[string]string{"A": "x"}})
	require.NoError(t, err)
	got, err := Parse(tagged)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "x", "B": "2"}, encoderTags(got).Freeform, "B was not listed, so it survives")

	tagged, err = Tag(tagged, Tags{Freeform: map[string]string{"B": ""}})
	require.NoError(t, err)
	got, err = Parse(tagged)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "x"}, encoderTags(got).Freeform, "an empty value removes the atom")
	assert.Empty(t, got.Title)
}

func TestCoverFormats(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)

	png := []byte("\x89PNG\r\n\x1a\nrest")
	out, err := Tag(m4a, Tags{Cover: png})
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, png, got.Cover)

	_, err = Tag(m4a, Tags{Cover: []byte("GIF89a")})
	require.ErrorIs(t, err, ErrCoverFormat)
}

func TestAudioSHA256StableAcrossTagging(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)
	before, err := AudioSHA256(m4a)
	require.NoError(t, err)
	require.Len(t, before, 64)

	tagged, err := Tag(m4a, Tags{
		Title: "A much longer title that grows the moov box", Cover: jpeg(),
		Freeform: map[string]string{"SPOTIFY_ID": "abc", "MIXTAPE_AUDIO_SHA256": before},
	})
	require.NoError(t, err)
	after, err := AudioSHA256(tagged)
	require.NoError(t, err)
	assert.Equal(t, before, after)
}

// box64 builds a top-level box with a 64-bit largesize header.
func box64(typ string, payload []byte) []byte {
	b := make([]byte, 16, 16+len(payload))
	binary.BigEndian.PutUint32(b[0:4], 1)
	copy(b[4:8], typ)
	binary.BigEndian.PutUint64(b[8:16], uint64(16+len(payload)))
	return append(b, payload...)
}

func box32(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b[0:4], uint32(8+len(payload)))
	copy(b[4:8], typ)
	return append(b, payload...)
}

func TestAudioSHA256HeaderForms(t *testing.T) {
	t.Parallel()
	audio := []byte("encoded audio payload")
	sum := sha256.Sum256(audio)
	want := hex.EncodeToString(sum[:])
	ftyp := box32("ftyp", []byte("M4A "))

	toEnd := append(bytes.Clone(ftyp), 0, 0, 0, 0, 'm', 'd', 'a', 't')
	toEnd = append(toEnd, audio...)

	tests := map[string][]byte{
		"32-bit":    append(bytes.Clone(ftyp), box32("mdat", audio)...),
		"largesize": append(bytes.Clone(ftyp), box64("mdat", audio)...),
		"to EOF":    toEnd,
	}
	for name, data := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := AudioSHA256(data)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			path := filepath.Join(t.TempDir(), "a.m4a")
			require.NoError(t, os.WriteFile(path, data, 0o600))
			got, err = HashFile(path)
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}

	_, err := AudioSHA256(ftyp)
	require.ErrorIs(t, err, errNoMdat)
}

func TestReadFileAndHashFile(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)
	tags := Tags{Title: "T", Artist: "A", Freeform: map[string]string{"SPOTIFY_ID": "id1"}, Cover: jpeg()}
	tagged, err := Tag(m4a, tags)
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "song.m4a")
	require.NoError(t, os.WriteFile(path, tagged, 0o600))

	got, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, tags, encoderTags(got))

	want, err := AudioSHA256(m4a)
	require.NoError(t, err)
	hash, err := HashFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, hash)
}

func TestReadFileErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := ReadFile(filepath.Join(dir, "missing.m4a"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = HashFile(filepath.Join(dir, "missing.m4a"))
	require.ErrorIs(t, err, os.ErrNotExist)

	noMoov := filepath.Join(dir, "nomoov.m4a")
	require.NoError(t, os.WriteFile(noMoov, box32("mdat", []byte("x")), 0o600))
	_, err = ReadFile(noMoov)
	require.ErrorIs(t, err, errNoMoov)

	noMdat := filepath.Join(dir, "nomdat.m4a")
	require.NoError(t, os.WriteFile(noMdat, box32("moov", nil), 0o600))
	_, err = HashFile(noMdat)
	require.ErrorIs(t, err, errNoMdat)

	bad := filepath.Join(dir, "bad.m4a")
	require.NoError(t, os.WriteFile(bad, []byte{0, 0, 0, 99, 'm', 'o', 'o', 'v'}, 0o600))
	_, err = ReadFile(bad)
	require.ErrorIs(t, err, errBadBox)
}

// afinfo fully parses the box structure: a broken moov or stale chunk
// offsets make it fail or report a wrong duration.
func TestTaggedFileStillValid(t *testing.T) {
	t.Parallel()
	out, err := Tag(makeM4A(t), Tags{
		Title: "T", Artist: "A", Comment: "spotify:track:x", TrackNumber: 1,
		Freeform: map[string]string{"SPOTIFY_ID": "x"}, Cover: jpeg(),
	})
	require.NoError(t, err)
	path := filepath.Join(t.TempDir(), "tagged.m4a")
	require.NoError(t, os.WriteFile(path, out, 0o600))
	info, err := exec.CommandContext(t.Context(), "afinfo", path).CombinedOutput()
	require.NoError(t, err, "afinfo rejected tagged file:\n%s", info)
}

func TestTagUnicode(t *testing.T) {
	t.Parallel()
	want := Tags{Title: "Ünïcødé — Song", Artist: "Sigur Rós", Album: "Ágætis byrjun"}
	out, err := Tag(makeM4A(t), want)
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, want, encoderTags(got))
}

// Track and disc numbers live in 16-bit fields; anything larger must be
// rejected rather than silently truncated.
func TestTagRejectsOutOfRangeNumbers(t *testing.T) {
	t.Parallel()
	m4a := makeM4A(t)
	for _, tags := range []Tags{
		{TrackNumber: 70000},
		{DiscTotal: -1, DiscNumber: 1},
	} {
		_, err := Tag(m4a, tags)
		require.ErrorIs(t, err, errOutOfRange, "%+v", tags)
	}
}

func TestTagRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		data []byte
		want error
	}{
		"box size below header": {data: []byte{0, 0, 0, 4, 'f', 't', 'y', 'p'}, want: errBadBox},
		"box overruns file":     {data: []byte{0, 0, 0, 64, 'f', 't', 'y', 'p'}, want: errBadBox},
		"short largesize":       {data: []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0}, want: errBadBox},
		"trailing bytes":        {data: []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e', 1, 2}, want: errTrailingBytes},
		"no moov":               {data: []byte{0, 0, 0, 8, 'm', 'd', 'a', 't'}, want: errNoMoov},
		"no mdat":               {data: []byte{0, 0, 0, 8, 'm', 'o', 'o', 'v'}, want: errNoMdat},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Tag(tc.data, Tags{Title: "x"})
			require.ErrorIs(t, err, tc.want)
		})
	}
}

// Write rewrites the file through a temp file; the result must keep the
// original permissions and carry the tags.
func TestWritePreservesModeAndTags(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "song.m4a")
	require.NoError(t, os.WriteFile(path, makeM4A(t), 0o640))
	require.NoError(t, Write(path, Tags{Title: "Kept"}))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o640), info.Mode().Perm())
	tags, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "Kept", tags.Title)

	require.Error(t, Write(filepath.Join(t.TempDir(), "missing.m4a"), Tags{}))
}
