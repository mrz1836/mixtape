package mp4tag

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// table32 is a stco payload: version and flags, entry count, entries.
func table32(entries ...uint32) []byte {
	p := binary.BigEndian.AppendUint32(make([]byte, 4, 8+4*len(entries)), uint32(len(entries)))
	for _, e := range entries {
		p = binary.BigEndian.AppendUint32(p, e)
	}
	return p
}

// table64 is a co64 payload: version and flags, entry count, entries.
func table64(entries ...uint64) []byte {
	p := binary.BigEndian.AppendUint32(make([]byte, 4, 8+8*len(entries)), uint32(len(entries)))
	for _, e := range entries {
		p = binary.BigEndian.AppendUint64(p, e)
	}
	return p
}

// withCount overwrites the entry count of a stco or co64 payload, to make
// it claim more entries than it holds.
func withCount(p []byte, n uint32) []byte {
	binary.BigEndian.PutUint32(p[4:8], n)
	return p
}

// entries32 reads the entries of a stco payload back.
func entries32(p []byte) []uint32 {
	out := make([]uint32, binary.BigEndian.Uint32(p[4:8]))
	for i := range out {
		out[i] = binary.BigEndian.Uint32(p[8+4*i:])
	}
	return out
}

// entries64 reads the entries of a co64 payload back.
func entries64(p []byte) []uint64 {
	out := make([]uint64, binary.BigEndian.Uint32(p[4:8]))
	for i := range out {
		out[i] = binary.BigEndian.Uint64(p[8+8*i:])
	}
	return out
}

// sampleTable wraps a chunk-offset box in the trak/mdia/minf/stbl chain
// where a track keeps it.
func sampleTable(table []byte) []byte {
	return testutil.Box("trak", testutil.Box("mdia", testutil.Box("minf", testutil.Box("stbl", table))))
}

func TestEntryCount(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		table []byte
		size  int
		want  int
		err   error
	}{
		"no entries":           {table: table32(), size: 4, want: 0},
		"exact fit":            {table: table32(7, 8, 9), size: 4, want: 3},
		"spare bytes":          {table: append(table32(7, 8), 0, 0, 0), size: 4, want: 2},
		"co64":                 {table: table64(7, 8), size: 8, want: 2},
		"short header":         {table: make([]byte, 7), size: 4, err: errBadBox},
		"over-counted":         {table: withCount(table32(7, 8), 3), size: 4, err: errBadBox},
		"stco read as co64":    {table: table32(7, 8, 9), size: 8, err: errBadBox},
		"count past any table": {table: withCount(table32(7), math.MaxUint32), size: 8, err: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n, err := entryCount(tc.table, tc.size)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, n)
		})
	}
}

// In these tests the audio used to start at 100: entries before it point
// somewhere else and stay, entries at or after it move with the audio.
const oldAudio = 100

func TestShiftOffsets32(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		shift int64
		want  []uint32
	}{
		"forward":     {shift: 50, want: []uint32{0, 99, 150, 250}},
		"backward":    {shift: -100, want: []uint32{0, 99, 0, 100}},
		"no shift":    {shift: 0, want: []uint32{0, 99, 100, 200}},
		"up to 4 GiB": {shift: math.MaxUint32 - 200, want: []uint32{0, 99, math.MaxUint32 - 100, math.MaxUint32}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := table32(0, 99, oldAudio, 200)
			require.NoError(t, shiftOffsets32(p, oldAudio, tc.shift))
			assert.Equal(t, tc.want, entries32(p))
		})
	}
}

func TestShiftOffsets32Rejects(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		table []byte
		shift int64
		err   error
	}{
		"past 4 GiB":   {table: table32(oldAudio, math.MaxUint32-10), shift: 11, err: errOffsetOverflow},
		"below zero":   {table: table32(oldAudio), shift: -oldAudio - 1, err: errOffsetOverflow},
		"short table":  {table: make([]byte, 4), shift: 1, err: errBadBox},
		"over-counted": {table: withCount(table32(oldAudio, 200), 3), shift: 1, err: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, shiftOffsets32(tc.table, oldAudio, tc.shift), tc.err)
		})
	}
}

func TestShiftOffsets64(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		shift int64
		want  []uint64
	}{
		"forward":  {shift: 50, want: []uint64{0, 99, 150, 1<<40 + 50}},
		"backward": {shift: -100, want: []uint64{0, 99, 0, 1<<40 - 100}},
		"no shift": {shift: 0, want: []uint64{0, 99, 100, 1 << 40}},
		// A co64 holds what a stco cannot.
		"past 4 GiB": {shift: math.MaxUint32, want: []uint64{0, 99, math.MaxUint32 + 100, 1<<40 + math.MaxUint32}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := table64(0, 99, oldAudio, 1<<40)
			require.NoError(t, shiftOffsets64(p, oldAudio, tc.shift))
			assert.Equal(t, tc.want, entries64(p))
		})
	}
}

func TestShiftOffsets64Rejects(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		table []byte
		shift int64
		err   error
	}{
		"above int64":  {table: table64(oldAudio, 1<<63), shift: 1, err: errOffsetOverflow},
		"below zero":   {table: table64(oldAudio), shift: -oldAudio - 1, err: errOffsetOverflow},
		"wraps int64":  {table: table64(math.MaxInt64 - 5), shift: 10, err: errOffsetOverflow},
		"short table":  {table: make([]byte, 4), shift: 1, err: errBadBox},
		"over-counted": {table: withCount(table64(oldAudio, 200), 3), shift: 1, err: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, shiftOffsets64(tc.table, oldAudio, tc.shift), tc.err)
		})
	}
}

func TestFixChunkOffsetsShiftsEveryTrack(t *testing.T) {
	t.Parallel()
	// A stco outside a track is not a chunk table, so it stays as it is.
	stray := testutil.Box("stco", table32(500))
	moov := testutil.Box("moov",
		testutil.FullBox("mvhd", make([]byte, 96)),
		sampleTable(testutil.Box("stco", table32(10, oldAudio, 300))),
		sampleTable(testutil.Box("co64", table64(oldAudio, 1<<33))),
		testutil.Box("udta", stray),
	)
	require.NoError(t, fixChunkOffsets(moov, oldAudio, 25))

	offsets, err := testutil.ChunkOffsets(moov)
	require.NoError(t, err)
	assert.Equal(t, []int64{10, 125, 325, 125, 1<<33 + 25}, offsets)
	assert.True(t, bytes.Contains(moov, stray))
}

func TestFixChunkOffsetsWithoutShiftIsANoOp(t *testing.T) {
	t.Parallel()
	// When the audio stays put no table needs reading, so even a broken
	// one is left alone without an error.
	moov := testutil.Box("moov", sampleTable(testutil.Box("stco", withCount(table32(oldAudio), 9))))
	before := bytes.Clone(moov)
	require.NoError(t, fixChunkOffsets(moov, oldAudio, 0))
	assert.Equal(t, before, moov)
}

func TestFixChunkOffsetsErrors(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		moov []byte
		err  error
	}{
		"broken track":        {moov: testutil.Box("moov", testutil.Box("trak", overrun("mdia"))), err: errBadBox},
		"broken sample table": {moov: testutil.Box("moov", sampleTable(overrun("stco"))), err: errBadBox},
		"over-counted stco": {
			moov: testutil.Box("moov", sampleTable(testutil.Box("stco", withCount(table32(oldAudio), 9)))),
			err:  errBadBox,
		},
		"stco past 4 GiB": {
			moov: testutil.Box("moov", sampleTable(testutil.Box("stco", table32(math.MaxUint32)))),
			err:  errOffsetOverflow,
		},
		"co64 above int64": {
			moov: testutil.Box("moov", sampleTable(testutil.Box("co64", table64(1<<63)))),
			err:  errOffsetOverflow,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			require.ErrorIs(t, fixChunkOffsets(tc.moov, oldAudio, 1), tc.err)
		})
	}
}

// variant is a named testutil.M4A layout.
type variant struct {
	name string
	opts testutil.M4AOptions
}

// oldItems are tags a file carries before it is retagged: managed atoms
// Tag replaces, and atoms it keeps.
func oldItems() [][]byte {
	return [][]byte{
		testutil.TextItem("\xa9nam", "An old title, long enough that dropping it shrinks moov"),
		testutil.TextItem("\xa9wrt", "Composer"),
		testutil.Box("covr", testutil.DataBox(dataJPEG, testutil.JPEG())),
		testutil.FreeformItem(FreeformMean, "SPOTIFY_ID", "old id"),
		testutil.FreeformItem("com.example.player", "SPOTIFY_ID", "theirs"),
	}
}

// variants returns every combination of the testutil.M4A options that
// decide where the audio and its chunk-offset table end up: table width,
// mdat header form, moov before or after mdat, free padding, and a udta
// that is absent, empty or tagged. A to-the-end mdat in front of moov
// swallows it, so those layouts have no moov.
func variants() []variant {
	mdats := []struct {
		name string
		hdr  testutil.MdatHeader
	}{{"32-bit mdat", testutil.Mdat32}, {"largesize mdat", testutil.MdatLarge}, {"to-the-end mdat", testutil.MdatToEnd}}
	udtas := []struct {
		name   string
		noUdta bool
		items  [][]byte
	}{{"no udta", true, nil}, {"empty ilst", false, nil}, {"tagged", false, oldItems()}}

	out := make([]variant, 0, 2*len(mdats)*2*2*len(udtas))
	for _, co64 := range []bool{false, true} {
		table := "stco"
		if co64 {
			table = "co64"
		}
		for _, m := range mdats {
			for _, moovLast := range []bool{false, true} {
				place := "moov first"
				if moovLast {
					place = "moov last"
				}
				for _, free := range []int{0, 40} {
					for _, u := range udtas {
						out = append(out, variant{
							name: fmt.Sprintf("%s, %s, %s, %d free, %s", table, m.name, place, free, u.name),
							opts: testutil.M4AOptions{
								CO64: co64, Mdat: m.hdr, MoovLast: moovLast, Free: free, NoUdta: u.noUdta, Items: u.items,
							},
						})
					}
				}
			}
		}
	}
	return out
}

// assertAudioIntact checks that tagging turned before into after without
// disturbing the audio: the mdat payload is the same, every chunk offset
// moved with it and points at the same bytes as before, and AudioSHA256
// and HashFile still report the payload's hash. It returns how far the
// payload moved.
func assertAudioIntact(t *testing.T, before, after []byte) int64 {
	t.Helper()
	audio, at, err := testutil.MdatPayload(before)
	require.NoError(t, err)
	offsets, err := testutil.ChunkOffsets(before)
	require.NoError(t, err)
	require.NotEmpty(t, offsets)

	// Chunk i of a single track runs from offset i to the next one, and the
	// last chunk to the end of the audio.
	end := at + int64(len(audio))
	chunks := make([][]byte, len(offsets))
	for i, off := range offsets {
		next := end
		if i+1 < len(offsets) {
			next = offsets[i+1]
		}
		require.True(t, at <= off && off <= next && next <= end, "chunk %d (%d..%d) lies in the audio", i, off, next)
		chunks[i] = before[off:next]
	}

	moved, newAt, err := testutil.MdatPayload(after)
	require.NoError(t, err)
	require.Equal(t, audio, moved, "the audio payload is copied unchanged")
	shift := newAt - at

	newOffsets, err := testutil.ChunkOffsets(after)
	require.NoError(t, err)
	require.Len(t, newOffsets, len(offsets))
	for i, off := range newOffsets {
		assert.Equal(t, offsets[i]+shift, off, "chunk %d moves with the audio", i)
		n := int64(len(chunks[i]))
		require.True(t, off >= 0 && off+n <= int64(len(after)), "chunk %d at %d lies in the file", i, off)
		assert.Equal(t, chunks[i], after[off:off+n], "chunk %d points at its audio", i)
	}

	want := audioHash(audio)
	got, err := AudioSHA256(after)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	got, err = HashFile(testutil.WriteFile(t, "tagged.m4a", after))
	require.NoError(t, err)
	assert.Equal(t, want, got)
	return shift
}

// The key invariant of Tag: whatever the layout, and whether moov grows or
// shrinks, every chunk offset still points at the audio it pointed at.
func TestTagKeepsChunkOffsetsOnTheAudio(t *testing.T) {
	t.Parallel()
	for _, v := range variants() {
		if v.opts.MoovLast && v.opts.Mdat == testutil.MdatToEnd {
			continue // no moov to retag
		}
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			for name, tags := range map[string]Tags{"all tags": fullTags(), "no tags": {}} {
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					file := testutil.M4A(v.opts)
					out, err := Tag(file, tags)
					require.NoError(t, err)
					shift := assertAudioIntact(t, file, out)
					assert.False(t, bytes.Contains(out, []byte("free")), "Tag drops free padding")

					// With moov last only the dropped padding moves the audio;
					// with moov first so does every byte moov grew or shrank.
					want := int64(len(out) - len(file))
					if v.opts.MoovLast {
						want = 0
						if v.opts.Free > 0 {
							want = -int64(8 + v.opts.Free)
						}
					}
					assert.Equal(t, want, shift)
					if !v.opts.MoovLast && name == "all tags" {
						assert.NotZero(t, shift, "the offsets had to be rewritten")
					}
				})
			}
		})
	}
}
