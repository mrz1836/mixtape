package mp4tag

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestChildBoxesMayUseLargesizeAndSizeZero(t *testing.T) {
	t.Parallel()
	data := testutil.M4A(testutil.M4AOptions{Items: [][]byte{
		testutil.Box64("\xa9nam", testutil.DataBox(dataUTF8, []byte("Largesize title"))),
		testutil.TextItem("\xa9ART", "Plain artist"),
		testutil.Box0("free", make([]byte, 6)), // padding to the end of ilst
	}})

	tags, err := Parse(data)
	require.NoError(t, err, "valid ISO-BMFF children are not malformed")
	assert.Equal(t, "Largesize title", tags.Title)
	assert.Equal(t, "Plain artist", tags.Artist)

	// Retagging keeps the size-0 padding, and the atoms written after it
	// must still be found: the kept box gets its real size.
	tagged, err := Tag(data, Tags{Album: "After the padding"})
	require.NoError(t, err)
	got, err := Parse(tagged)
	require.NoError(t, err)
	assert.Equal(t, Tags{Album: "After the padding"}, got)
}

func TestSizeZeroMoovChildBeforeTheRebuiltUdta(t *testing.T) {
	t.Parallel()
	// A size-0 box as moov's last child: udta is rebuilt after it.
	data := withMoov(
		testutil.FullBox("mvhd", make([]byte, 96)),
		udtaWith(testutil.TextItem("\xa9nam", "Title")),
		testutil.Box0("free", make([]byte, 4)),
	)
	tagged, err := Tag(data, Tags{Title: "New"})
	require.NoError(t, err)
	got, err := Parse(tagged)
	require.NoError(t, err)
	assert.Equal(t, "New", got.Title)
}

func TestRetagIsIdempotent(t *testing.T) {
	t.Parallel()
	data := testutil.M4A(testutil.M4AOptions{Items: [][]byte{testutil.TextItem("\xa9nam", "Old")}})
	tags := fullTags()
	once, err := Tag(data, tags)
	require.NoError(t, err)
	twice, err := Tag(once, tags)
	require.NoError(t, err)
	assert.Equal(t, once, twice, "no managed atom is kept beside its rebuilt copy")

	got, err := Parse(twice)
	require.NoError(t, err)
	assert.Equal(t, tags, got)
}

func TestSixByteDiscAtomIsRead(t *testing.T) {
	t.Parallel()
	// iTunes writes disk with 6 value bytes: reserved, number, total.
	disk := testutil.Box("disk", testutil.DataBox(0, []byte{0, 0, 0, 2, 0, 3}))
	got, err := Parse(testutil.M4A(testutil.M4AOptions{Items: [][]byte{disk}}))
	require.NoError(t, err)
	assert.Equal(t, 2, got.DiscNumber)
	assert.Equal(t, 3, got.DiscTotal)
}

func TestEveryManagedAtomIsRecognized(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, a := range tagAtoms() {
		assert.False(t, seen[a.typ], "%q is listed once", a.typ)
		seen[a.typ] = true
		var typ [4]byte
		copy(typ[:], a.typ)
		found, ok := findTagAtom(typ)
		require.True(t, ok, a.typ)
		assert.Equal(t, a.typ, found.typ)
	}
	_, ok := findTagAtom([4]byte{'-', '-', '-', '-'})
	assert.False(t, ok, "freeform atoms are handled apart")
}

func TestRawGivesSizeZeroBoxesTheirSize(t *testing.T) {
	t.Parallel()
	b := box{typ: [4]byte{'f', 'r', 'e', 'e'}, data: testutil.Box0("free", []byte{1, 2, 3}), hdr: 8}
	raw := b.raw()
	assert.Equal(t, uint32(11), binary.BigEndian.Uint32(raw[0:4]))
	assert.Equal(t, uint32(0), binary.BigEndian.Uint32(b.data[0:4]), "the source is not modified")

	sized := box{data: testutil.Box("free", []byte{1}), hdr: 8}
	assert.True(t, bytes.Equal(sized.data, sized.raw()))
}
