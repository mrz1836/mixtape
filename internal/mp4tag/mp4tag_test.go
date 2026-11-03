package mp4tag

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// gaplessAtom is the freeform atom (encoder delay and padding) afconvert
// writes into every file it encodes.
const gaplessAtom = "iTunSMPB"

// encoderTags drops the gapless atom afconvert writes, so assertions on a
// real file see only what the test set. TestRealFileRoundTrip checks that
// the atom survives retagging.
func encoderTags(tags Tags) Tags {
	delete(tags.Freeform, gaplessAtom)
	if len(tags.Freeform) == 0 {
		tags.Freeform = nil
	}
	return tags
}

// fullTags sets every field Tags manages, so a round trip checks them all.
func fullTags() Tags {
	return Tags{
		Title:       "Still Loving You",
		Artist:      "Scorpions",
		AlbumArtist: "Scorpions",
		Album:       "Love at First Sting",
		Grouping:    "Late Night",
		Comment:     "https://open.spotify.com/track/abc123",
		TrackNumber: 7,
		TrackTotal:  9,
		DiscNumber:  1,
		DiscTotal:   2,
		Date:        "1984-03-27",
		Freeform:    map[string]string{"SPOTIFY_ID": "abc123", "MIXTAPE_AUDIO_SHA256": "ff00"},
		Cover:       testutil.JPEG(),
	}
}

// withMoov lays out ftyp, a moov holding children, and a short mdat, moov
// first as afconvert writes it. It has no track unless children add one,
// which lets a test put any box anywhere in moov.
func withMoov(children ...[]byte) []byte {
	return slices.Concat(
		testutil.Box("ftyp", []byte("M4A ")),
		testutil.Box("moov", children...),
		testutil.Box("mdat", []byte("audio")),
	)
}

// udtaWith is a udta holding an iTunes meta whose ilst has items.
func udtaWith(items ...[]byte) []byte {
	return testutil.Box("udta", testutil.FullBox("meta", mdirHandler(), testutil.Box("ilst", items...)))
}

// mdirHandler is the hdlr box that marks a meta as iTunes metadata.
func mdirHandler() []byte {
	return testutil.FullBox("hdlr", make([]byte, 4), []byte("mdir"), make([]byte, 13))
}

// overrun is a lone box header of type typ claiming 2 GiB, more than any
// container in these tests holds.
func overrun(typ string) []byte {
	return append([]byte{0x7F, 0xFF, 0xFF, 0xFF}, typ...)
}

// audioHash is the hex SHA-256 of an mdat payload, computed without
// mp4tag so its hashing functions have something to agree with.
func audioHash(audio []byte) string {
	sum := sha256.Sum256(audio)
	return hex.EncodeToString(sum[:])
}

// malformedInputs are files Tag must refuse, with the error each one gets.
// The fuzz targets start from them too.
func malformedInputs() map[string]struct {
	data []byte
	err  error
} {
	// A one-chunk file whose chunk-offset table edit corrupts. The file is
	// laid out moov first, so retagging it moves the audio and the table
	// must be rewritten.
	table := func(o testutil.M4AOptions, edit func(table []byte)) []byte {
		o.Chunks = 1
		file := testutil.M4A(o)
		typ := "stco"
		if o.CO64 {
			typ = "co64"
		}
		at := bytes.Index(file, []byte(typ))
		size := int(binary.BigEndian.Uint32(file[at-4 : at]))
		edit(file[at+4 : at-4+size])
		return file
	}
	count := func(n uint32) func([]byte) {
		return func(p []byte) { binary.BigEndian.PutUint32(p[4:8], n) }
	}
	largesize := []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}

	return map[string]struct {
		data []byte
		err  error
	}{
		"empty":                 {data: nil, err: errNoMoov},
		"box size below header": {data: []byte{0, 0, 0, 4, 'f', 't', 'y', 'p'}, err: errBadBox},
		"box overruns file":     {data: []byte{0, 0, 0, 64, 'f', 't', 'y', 'p'}, err: errBadBox},
		"short largesize":       {data: []byte{0, 0, 0, 1, 'm', 'd', 'a', 't', 0, 0}, err: errBadBox},
		"largesize above int64": {data: largesize, err: errBadBox},
		"trailing bytes":        {data: []byte{0, 0, 0, 8, 'f', 'r', 'e', 'e', 1, 2}, err: errTrailingBytes},
		"no moov":               {data: testutil.Box("mdat", []byte("audio")), err: errNoMoov},
		"no mdat":               {data: testutil.Box("moov"), err: errNoMdat},
		"moov child overruns":   {data: withMoov(overrun("udta")), err: errBadBox},
		"udta child overruns":   {data: withMoov(testutil.Box("udta", overrun("meta"))), err: errBadBox},
		"meta without version":  {data: withMoov(testutil.Box("udta", testutil.Box("meta", []byte{0, 0}))), err: errBadBox},
		"meta child overruns":   {data: withMoov(testutil.Box("udta", testutil.FullBox("meta", overrun("ilst")))), err: errBadBox},
		"ilst child overruns":   {data: withMoov(udtaWith(overrun("\xa9nam"))), err: errBadBox},
		"track child overruns":  {data: withMoov(testutil.Box("trak", overrun("mdia"))), err: errBadBox},
		"over-counted stco":     {data: table(testutil.M4AOptions{}, count(9)), err: errBadBox},
		"over-counted co64":     {data: table(testutil.M4AOptions{CO64: true}, count(9)), err: errBadBox},
		"stco entry at 4 GiB": {
			data: table(testutil.M4AOptions{}, func(p []byte) { binary.BigEndian.PutUint32(p[8:12], math.MaxUint32) }),
			err:  errOffsetOverflow,
		},
		"co64 entry above int64": {
			data: table(testutil.M4AOptions{CO64: true}, func(p []byte) { binary.BigEndian.PutUint64(p[8:16], 1<<63) }),
			err:  errOffsetOverflow,
		},
	}
}

func TestTagRoundTrip(t *testing.T) {
	t.Parallel()
	want := fullTags()
	out, err := Tag(testutil.M4A(testutil.M4AOptions{}), want)
	require.NoError(t, err)

	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, want, got)

	// The independent reader finds every atom, in the order Tag writes them.
	types, err := testutil.IlstTypes(out)
	require.NoError(t, err)
	assert.Equal(t, []string{
		"\xa9nam", "\xa9ART", "aART", "\xa9alb", "\xa9grp", "\xa9cmt", "\xa9day", "trkn", "disk", "covr", "----", "----",
	}, types)
}

func TestEmptyTagsRemoveEveryManagedAtom(t *testing.T) {
	t.Parallel()
	tagged, err := Tag(testutil.M4A(testutil.M4AOptions{}), fullTags())
	require.NoError(t, err)
	out, err := Tag(tagged, Tags{})
	require.NoError(t, err)

	// Empty Tags list no freeform names, so the freeform atoms stay.
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, Tags{Freeform: fullTags().Freeform}, got)
	types, err := testutil.IlstTypes(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"----", "----"}, types)
}

func TestTagOverwriteExisting(t *testing.T) {
	t.Parallel()
	first, err := Tag(testutil.M4A(testutil.M4AOptions{}),
		Tags{Title: "First", Artist: "A", TrackNumber: 1, Cover: testutil.JPEG()})
	require.NoError(t, err)
	second, err := Tag(first, Tags{Title: "Second", Artist: "B", Album: "C"})
	require.NoError(t, err)

	got, err := Parse(second)
	require.NoError(t, err)
	assert.Equal(t, Tags{Title: "Second", Artist: "B", Album: "C"}, got,
		"managed atoms, cover included, are replaced")
}

// Managed atoms are rebuilt rather than read, so a broken one that makes
// Parse fail does not stop Tag from replacing it.
func TestTagReplacesUnreadableManagedAtoms(t *testing.T) {
	t.Parallel()
	file := testutil.M4A(testutil.M4AOptions{Items: [][]byte{testutil.Box("\xa9nam", overrun("data"))}})
	_, err := Parse(file)
	require.ErrorIs(t, err, errBadBox)

	out, err := Tag(file, Tags{Title: "Fixed"})
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, Tags{Title: "Fixed"}, got)
}

func TestFreeformRoundTrip(t *testing.T) {
	t.Parallel()
	// Atoms of another namespace and malformed ones are not this package's
	// to read or replace, even under a listed name.
	foreign := testutil.FreeformItem("com.example.player", "A", "theirs")
	nameless := testutil.Box("----", testutil.FullBox("mean", []byte(FreeformMean)), testutil.DataBox(dataUTF8, []byte("v")))
	file := testutil.M4A(testutil.M4AOptions{Items: [][]byte{
		testutil.FreeformItem(FreeformMean, "A", "1"),
		testutil.FreeformItem(FreeformMean, "B", "2"),
		testutil.FreeformItem(FreeformMean, "C", "3"),
		foreign,
		nameless,
	}})
	got, err := Parse(file)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "1", "B": "2", "C": "3"}, got.Freeform)

	out, err := Tag(file, Tags{Freeform: map[string]string{"A": "x", "B": "", "D": "4"}})
	require.NoError(t, err)
	got, err = Parse(out)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"A": "x", "C": "3", "D": "4"}, got.Freeform,
		"A is replaced, B removed by its empty value, C kept because it is not listed, D added")
	assert.True(t, bytes.Contains(out, foreign), "another namespace's A is kept as is")
	assert.True(t, bytes.Contains(out, nameless), "a malformed atom is kept as is")

	// C, foreign and nameless stay in place; A and D follow, sorted by name.
	types, err := testutil.IlstTypes(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"----", "----", "----", "----", "----"}, types)
}

func TestCoverFormats(t *testing.T) {
	t.Parallel()
	file := testutil.M4A(testutil.M4AOptions{})
	for name, tc := range map[string]struct {
		img  []byte
		code uint32
	}{
		"jpeg": {img: testutil.JPEG(), code: dataJPEG},
		"png":  {img: testutil.PNG(), code: dataPNG},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, err := Tag(file, Tags{Cover: tc.img})
			require.NoError(t, err)
			got, err := Parse(out)
			require.NoError(t, err)
			assert.Equal(t, tc.img, got.Cover)
			assert.True(t, bytes.Contains(out, testutil.Box("covr", testutil.DataBox(tc.code, tc.img))),
				"the data atom's type matches the image format")
		})
	}
	for name, img := range map[string][]byte{
		"gif":            []byte("GIF89a"),
		"truncated jpeg": {0xFF, 0xD8},
		"text":           []byte("not an image"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Tag(file, Tags{Cover: img})
			require.ErrorIs(t, err, ErrCoverFormat)
		})
	}
}

func TestTagCreatesMetaWhenMissing(t *testing.T) {
	t.Parallel()
	mvhd := testutil.FullBox("mvhd", make([]byte, 96))
	copyright := testutil.Box("cprt", []byte("kept as is"))
	for name, tc := range map[string]struct {
		file []byte
		keep []byte // a box that must come through untouched
	}{
		"no udta":           {file: testutil.M4A(testutil.M4AOptions{NoUdta: true}), keep: mvhd},
		"udta without meta": {file: withMoov(mvhd, testutil.Box("udta", copyright)), keep: copyright},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			want := Tags{Title: "T", Cover: testutil.PNG()}
			out, err := Tag(tc.file, want)
			require.NoError(t, err)
			got, err := Parse(out)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			types, err := testutil.IlstTypes(out)
			require.NoError(t, err, "moov/udta/meta/ilst is in place")
			assert.Equal(t, []string{"\xa9nam", "covr"}, types)
			assert.True(t, bytes.Contains(out, mdirHandler()), "the new meta declares the iTunes metadata handler")
			assert.True(t, bytes.Contains(out, tc.keep))
		})
	}
}

func TestUnmanagedIlstAtomsArePreserved(t *testing.T) {
	t.Parallel()
	composer := testutil.TextItem("\xa9wrt", "Composer")
	encoder := testutil.TextItem("\xa9too", "Encoder 1.0")
	opaque := testutil.Box("xid ", []byte("opaque payload"))
	file := testutil.M4A(testutil.M4AOptions{Items: [][]byte{
		composer,
		testutil.TextItem("\xa9nam", "Old"),
		encoder,
		opaque,
		testutil.Box("covr", testutil.DataBox(dataJPEG, testutil.JPEG())),
	}})

	out, err := Tag(file, Tags{Title: "New", Artist: "A"})
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, Tags{Title: "New", Artist: "A"}, got)

	// Unmanaged atoms keep their order ahead of the rebuilt managed ones.
	types, err := testutil.IlstTypes(out)
	require.NoError(t, err)
	assert.Equal(t, []string{"\xa9wrt", "\xa9too", "xid ", "\xa9nam", "\xa9ART"}, types)
	for _, atom := range [][]byte{composer, encoder, opaque} {
		assert.True(t, bytes.Contains(out, atom), "%q is copied byte for byte", atom[4:8])
	}
}

func TestParseIgnoresUnusableValues(t *testing.T) {
	t.Parallel()
	file := testutil.M4A(testutil.M4AOptions{Items: [][]byte{
		testutil.Box("\xa9nam"), // no data child
		testutil.Box("\xa9ART", testutil.Box("data", []byte{0, 0, 0, dataUTF8})), // no room for type and locale
		testutil.Box("\xa9alb", testutil.DataBox(0, []byte("binary"))),           // not UTF-8 text
		testutil.Box("covr", testutil.DataBox(dataUTF8, testutil.JPEG())),        // not an image type
		testutil.Box("trkn", testutil.DataBox(0, []byte{0, 0, 0, 1})),            // too short for number and total
		testutil.FreeformItem("com.example.player", "X", "another namespace"),
		testutil.Box("----", // a value that is not UTF-8 text
			testutil.FullBox("mean", []byte(FreeformMean)), testutil.FullBox("name", []byte("Y")),
			testutil.DataBox(0, []byte("binary"))),
		testutil.Box("----", // a mean too short to hold its version and flags
			testutil.Box("mean", []byte{1}), testutil.FullBox("name", []byte("Z")),
			testutil.DataBox(dataUTF8, []byte("v"))),
	}})
	got, err := Parse(file)
	require.NoError(t, err)
	assert.Equal(t, Tags{}, got)
}

func TestParseRejectsMalformedMoov(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		data []byte
		err  error
	}{
		"empty":                    {data: nil, err: errNoMoov},
		"bad top-level box":        {data: overrun("moov"), err: errBadBox},
		"udta child overruns":      {data: withMoov(testutil.Box("udta", overrun("meta"))), err: errBadBox},
		"meta without version":     {data: withMoov(testutil.Box("udta", testutil.Box("meta", []byte{0, 0}))), err: errBadBox},
		"tag atom data overruns":   {data: withMoov(udtaWith(testutil.Box("\xa9nam", overrun("data")))), err: errBadBox},
		"track child overruns":     {data: withMoov(testutil.Box("trak", overrun("mdia"))), err: errBadBox},
		"meta outside udta broken": {data: withMoov(testutil.Box("meta", []byte{0})), err: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Parse(tc.data)
			require.ErrorIs(t, err, tc.err)
		})
	}
}

func TestTagUnicode(t *testing.T) {
	t.Parallel()
	want := Tags{Title: "Ünïcødé — Song", Artist: "Sigur Rós", Album: "Ágætis byrjun"}
	out, err := Tag(testutil.M4A(testutil.M4AOptions{}), want)
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

// Track and disc numbers live in 16-bit fields: either half of a pair may
// be set alone, up to the limit, and anything outside it is rejected
// rather than silently truncated.
func TestTagNumberRange(t *testing.T) {
	t.Parallel()
	file := testutil.M4A(testutil.M4AOptions{})
	for _, tags := range []Tags{
		{TrackNumber: 3},
		{TrackTotal: 12},
		{DiscNumber: math.MaxUint16, DiscTotal: math.MaxUint16},
	} {
		out, err := Tag(file, tags)
		require.NoError(t, err, "%+v", tags)
		got, err := Parse(out)
		require.NoError(t, err)
		assert.Equal(t, tags, got)
	}
	for _, tags := range []Tags{
		{TrackNumber: math.MaxUint16 + 1},
		{TrackTotal: -1},
		{DiscNumber: -1},
		{DiscNumber: 1, DiscTotal: 70000},
	} {
		_, err := Tag(file, tags)
		require.ErrorIs(t, err, errOutOfRange, "%+v", tags)
	}
}

func TestTagRejectsMalformedInput(t *testing.T) {
	t.Parallel()
	for name, tc := range malformedInputs() {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Tag(tc.data, Tags{Title: "x"})
			require.ErrorIs(t, err, tc.err)
		})
	}
}

func TestAudioSHA256HeaderForms(t *testing.T) {
	t.Parallel()
	audio := []byte("encoded audio payload")
	want := audioHash(audio)
	ftyp := testutil.Box("ftyp", []byte("M4A "))
	for name, data := range map[string][]byte{
		"32-bit":    slices.Concat(ftyp, testutil.Box("mdat", audio)),
		"largesize": slices.Concat(ftyp, testutil.Box64("mdat", audio)),
		"to EOF":    slices.Concat(ftyp, testutil.Box0("mdat", audio)),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := AudioSHA256(data)
			require.NoError(t, err)
			assert.Equal(t, want, got)

			got, err = HashFile(testutil.WriteFile(t, "a.m4a", data))
			require.NoError(t, err)
			assert.Equal(t, want, got)
		})
	}

	_, err := AudioSHA256(ftyp)
	require.ErrorIs(t, err, errNoMdat)
	_, err = AudioSHA256(overrun("mdat"))
	require.ErrorIs(t, err, errBadBox)
}

func TestReadFileAndHashFile(t *testing.T) {
	t.Parallel()
	tags := Tags{Title: "T", Artist: "A", Freeform: map[string]string{"SPOTIFY_ID": "id1"}, Cover: testutil.JPEG()}
	for name, o := range map[string]testutil.M4AOptions{
		"moov first":  {},
		"moov last":   {MoovLast: true, CO64: true},
		"largesize":   {Mdat: testutil.MdatLarge, Free: 16},
		"mdat to end": {Mdat: testutil.MdatToEnd},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			file := testutil.M4A(o)
			tagged, err := Tag(file, tags)
			require.NoError(t, err)
			path := testutil.WriteFile(t, "song.m4a", tagged)

			got, err := ReadFile(path)
			require.NoError(t, err)
			assert.Equal(t, tags, got)

			audio, _, err := testutil.MdatPayload(file)
			require.NoError(t, err)
			hash, err := HashFile(path)
			require.NoError(t, err)
			assert.Equal(t, audioHash(audio), hash)
		})
	}
}

func TestReadFileAndHashFileErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing.m4a")
	_, err := ReadFile(missing)
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = HashFile(missing)
	require.ErrorIs(t, err, os.ErrNotExist)

	// A directory opens like a file, but reading it fails.
	_, err = ReadFile(dir)
	require.Error(t, err)
	_, err = HashFile(dir)
	require.Error(t, err)

	read := func(path string) error {
		_, readErr := ReadFile(path)
		return readErr
	}
	hash := func(path string) error {
		_, hashErr := HashFile(path)
		return hashErr
	}
	for name, tc := range map[string]struct {
		fn   func(path string) error
		data []byte
		want error
	}{
		"ReadFile of an empty file": {fn: read, data: nil, want: errNoMoov},
		"ReadFile without moov":     {fn: read, data: testutil.Box("mdat", []byte("x")), want: errNoMoov},
		"ReadFile of a bad header":  {fn: read, data: overrun("moov"), want: errBadBox},
		"ReadFile of a broken moov": {fn: read, data: withMoov(testutil.Box("udta", overrun("meta"))), want: errBadBox},
		"HashFile of an empty file": {fn: hash, data: nil, want: errNoMdat},
		"HashFile without mdat":     {fn: hash, data: testutil.Box("moov"), want: errNoMdat},
		"HashFile of a bad header":  {fn: hash, data: slices.Concat(testutil.Box("moov"), overrun("mdat")), want: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, tc.fn(testutil.WriteFile(t, "f.m4a", tc.data)), tc.want)
		})
	}
}

// ReadFile loads moov into memory, so it refuses one larger than
// maxMoovSize instead of allocating whatever the header claims.
func TestReadFileMoovSizeGuard(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		size int64
		err  error
	}{
		"at the limit":   {size: maxMoovSize},
		"past the limit": {size: maxMoovSize + 1, err: errTooLarge},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// A moov header, then zeros up to its claimed size (a sparse
			// file). The zeros read as one empty child box, which holds no
			// tags.
			path := testutil.WriteFile(t, "big.m4a", append(binary.BigEndian.AppendUint32(nil, uint32(tc.size)), "moov"...))
			require.NoError(t, os.Truncate(path, tc.size))

			got, err := ReadFile(path)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, Tags{}, got)
		})
	}
}

// The tests below run on real afconvert files, AAC and ALAC, and skip
// where the encoders are unavailable (Linux CI, sandboxes). They check what
// synthetic files cannot: that real encoder output survives tagging.

// eachRealFile runs fn as a parallel subtest per codec Mixtape encodes
// to, with a real file.
func eachRealFile(t *testing.T, fn func(t *testing.T, codec Codec, file []byte)) {
	t.Helper()
	for _, name := range []string{"aac", "alac"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			codec, err := ParseCodec(name)
			require.NoError(t, err)
			fn(t, codec, testutil.RealM4A(t, name))
		})
	}
}

func TestRealFileRoundTrip(t *testing.T) {
	t.Parallel()
	eachRealFile(t, realFileRoundTrip)
}

func realFileRoundTrip(t *testing.T, _ Codec, file []byte) {
	orig, err := Parse(file)
	require.NoError(t, err)
	require.Contains(t, orig.Freeform, gaplessAtom, "afconvert writes gapless info")

	want := fullTags()
	out, err := Tag(file, want)
	require.NoError(t, err)
	got, err := Parse(out)
	require.NoError(t, err)
	assert.Equal(t, orig.Freeform[gaplessAtom], got.Freeform[gaplessAtom], "an unlisted freeform atom survives")
	assert.Equal(t, want, encoderTags(got))
	assertAudioIntact(t, file, out)

	// afinfo parses the whole box structure: a broken moov or stale chunk
	// offsets make it fail.
	path := testutil.WriteFile(t, "tagged.m4a", out)
	info, err := exec.CommandContext(t.Context(), "afinfo", path).CombinedOutput()
	require.NoError(t, err, "afinfo rejected the tagged file:\n%s", info)
}

func TestRealFileAudioSHA256StableAcrossTagging(t *testing.T) {
	t.Parallel()
	eachRealFile(t, realFileHashStable)
}

func realFileHashStable(t *testing.T, _ Codec, file []byte) {
	before, err := AudioSHA256(file)
	require.NoError(t, err)
	require.Len(t, before, 64)

	tags := fullTags()
	tags.Title = "A much longer title that grows the moov box"
	tags.Freeform["MIXTAPE_AUDIO_SHA256"] = before
	tagged, err := Tag(file, tags)
	require.NoError(t, err)
	after, err := AudioSHA256(tagged)
	require.NoError(t, err)
	assert.Equal(t, before, after)

	shrunk, err := Tag(tagged, Tags{Title: "T"})
	require.NoError(t, err)
	after, err = AudioSHA256(shrunk)
	require.NoError(t, err)
	assert.Equal(t, before, after, "a retag that shrinks moov keeps the audio too")
}

func TestRealFileReadFileAndHashFile(t *testing.T) {
	t.Parallel()
	eachRealFile(t, realFileReadAndHash)
}

func realFileReadAndHash(t *testing.T, codec Codec, file []byte) {
	tags := Tags{Title: "T", Artist: "A", Freeform: map[string]string{"SPOTIFY_ID": "id1"}, Cover: testutil.JPEG()}
	tagged, err := Tag(file, tags)
	require.NoError(t, err)
	path := testutil.WriteFile(t, "song.m4a", tagged)

	got, err := ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, tags, encoderTags(got))

	want, err := AudioSHA256(file)
	require.NoError(t, err)
	hash, err := HashFile(path)
	require.NoError(t, err)
	assert.Equal(t, want, hash)

	got2, err := ReadCodec(path)
	require.NoError(t, err)
	assert.Equal(t, codec, got2, "the codec reads back from the tagged file")
}

// TagFile streams a real file to the same bytes Tag returns, and afinfo
// accepts the result.
func TestRealFileTagFile(t *testing.T) {
	t.Parallel()
	eachRealFile(t, func(t *testing.T, _ Codec, file []byte) {
		want, err := Tag(file, fullTags())
		require.NoError(t, err)
		path := testutil.WriteFile(t, "song.m4a", file)
		got := tagFileBytes(t, path, fullTags())
		assert.Equal(t, want, got)
		out := testutil.WriteFile(t, "tagged.m4a", got)
		info, err := exec.CommandContext(t.Context(), "afinfo", out).CombinedOutput()
		require.NoError(t, err, "afinfo rejected the streamed file:\n%s", info)
	})
}
