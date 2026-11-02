package mp4tag

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// box0 builds a box whose size field is 0: "to the end of its container".
func box0(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	copy(b[4:8], typ)
	return append(b, payload...)
}

// textItem is an ilst text atom built with box as its outer header.
func textItem(box func(string, []byte) []byte, typ, val string) []byte {
	data := make([]byte, 8, 8+len(val))
	binary.BigEndian.PutUint32(data[0:4], dataUTF8)
	return box(typ, box32("data", append(data, val...)))
}

// fileWithIlst is a minimal M4A whose ilst holds items, inside moov ->
// udta -> meta, followed by moovTail (more moov children) and mdat.
func fileWithIlst(items, moovTail []byte) []byte {
	hdlr := box32("hdlr", append(make([]byte, 8), "mdir\x00\x00\x00\x00\x00\x00\x00\x00\x00"...))
	meta := box32("meta", append(append([]byte{0, 0, 0, 0}, hdlr...), box32("ilst", items)...))
	moov := box32("moov", append(append(box32("mvhd", make([]byte, 100)), box32("udta", meta)...), moovTail...))
	out := append(box32("ftyp", []byte("M4A \x00\x00\x02\x00")), moov...)
	return append(out, box32("mdat", []byte("audio"))...)
}

func TestChildBoxesMayUseLargesizeAndSizeZero(t *testing.T) {
	t.Parallel()
	items := append(textItem(box64, "\xa9nam", "Largesize title"), textItem(box32, "\xa9ART", "Plain artist")...)
	items = append(items, box0("free", make([]byte, 6))...) // padding to the end of ilst
	data := fileWithIlst(items, nil)

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
	data := fileWithIlst(textItem(box32, "\xa9nam", "Title"), box0("free", make([]byte, 4)))
	tagged, err := Tag(data, Tags{Title: "New"})
	require.NoError(t, err)
	got, err := Parse(tagged)
	require.NoError(t, err)
	assert.Equal(t, "New", got.Title)
}

func TestRetagIsIdempotent(t *testing.T) {
	t.Parallel()
	data := fileWithIlst(textItem(box32, "\xa9nam", "Old"), nil)
	tags := Tags{
		Title: "Title", Artist: "Artist", AlbumArtist: "Album Artist", Album: "Album", Grouping: "Group",
		Comment: "Comment", Date: "1984-02-23", TrackNumber: 3, TrackTotal: 9, DiscNumber: 1, DiscTotal: 2,
		Freeform: map[string]string{"SPOTIFY_ID": "id"}, Cover: jpeg(),
	}
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
	val := []byte{0, 0, 0, 2, 0, 3}
	data := make([]byte, 8, 8+len(val))
	disk := box32("disk", box32("data", append(data, val...)))
	got, err := Parse(fileWithIlst(disk, nil))
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
	b := box{typ: [4]byte{'f', 'r', 'e', 'e'}, data: box0("free", []byte{1, 2, 3}), hdr: 8}
	raw := b.raw()
	assert.Equal(t, uint32(11), binary.BigEndian.Uint32(raw[0:4]))
	assert.Equal(t, uint32(0), binary.BigEndian.Uint32(b.data[0:4]), "the source is not modified")

	sized := box{data: box32("free", []byte{1}), hdr: 8}
	assert.True(t, bytes.Equal(sized.data, sized.raw()))
}
