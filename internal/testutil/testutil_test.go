package testutil

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/wav"
)

var errEncode = errors.New("encoder exploded")

func TestBoxHeaders(t *testing.T) {
	t.Parallel()
	assert.Equal(t, []byte{0, 0, 0, 10, 'f', 'r', 'e', 'e', 1, 2}, Box("free", []byte{1}, []byte{2}))
	assert.Equal(t, []byte{0, 0, 0, 1, 'f', 'r', 'e', 'e', 0, 0, 0, 0, 0, 0, 0, 17, 1}, Box64("free", []byte{1}))
	assert.Equal(t, []byte{0, 0, 0, 0, 'f', 'r', 'e', 'e', 1}, Box0("free", []byte{1}))
	assert.Equal(t, []byte{0, 0, 0, 13, 'm', 'e', 'a', 'n', 0, 0, 0, 0, 'x'}, FullBox("mean", []byte("x")))
}

func TestM4AChunkOffsetsPointIntoTheAudio(t *testing.T) {
	t.Parallel()
	audio := []byte("0123456789abcdef")
	for name, o := range map[string]M4AOptions{
		"default":                        {},
		"co64":                           {CO64: true},
		"largesize":                      {Mdat: MdatLarge},
		"mdat to end":                    {Mdat: MdatToEnd},
		"moov last":                      {MoovLast: true},
		"moov last, but mdat to the end": {MoovLast: true, Mdat: MdatToEnd},
		"with padding":                   {Free: 32},
		"no udta":                        {NoUdta: true},
		"existing tags":                  {Items: [][]byte{TextItem("\xa9nam", "Title")}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			o.Audio = audio
			file := M4A(o)
			payload, at, err := MdatPayload(file)
			require.NoError(t, err)
			assert.Equal(t, audio, payload)
			offsets, err := ChunkOffsets(file)
			require.NoError(t, err)
			require.Len(t, offsets, 4)
			for i, off := range offsets {
				assert.Equal(t, at+int64(i*4), off, "chunk %d", i)
				assert.Equal(t, audio[i*4:i*4+4], file[off:off+4])
			}
		})
	}
}

func TestM4ADefaultsAndItems(t *testing.T) {
	t.Parallel()
	file := M4A(M4AOptions{Items: [][]byte{TextItem("\xa9nam", "T"), FreeformItem("com.apple.iTunes", "X", "v")}})
	payload, _, err := MdatPayload(file)
	require.NoError(t, err)
	assert.Len(t, payload, 128)
	types, err := IlstTypes(file)
	require.NoError(t, err)
	assert.Equal(t, []string{"\xa9nam", "----"}, types)

	types, err = IlstTypes(M4A(M4AOptions{NoUdta: true}))
	require.NoError(t, err)
	assert.Empty(t, types)
}

func TestReaderRejectsBrokenFiles(t *testing.T) {
	t.Parallel()
	for name, file := range map[string][]byte{
		"overrun":         {0, 0, 0, 64, 'm', 'd', 'a', 't'},
		"short largesize": {0, 0, 0, 1, 'm', 'd', 'a', 't', 0},
	} {
		_, _, err := MdatPayload(file)
		require.ErrorIs(t, err, ErrBadFile, name)
	}
	_, _, err := MdatPayload(Box("free"))
	require.ErrorIs(t, err, ErrBadFile, "no mdat")

	bad := Box("moov", Box("trak", Box("mdia", Box("minf", Box("stbl", FullBox("stco", []byte{0, 0, 0, 9}))))))
	_, err = ChunkOffsets(bad)
	require.ErrorIs(t, err, ErrBadFile, "more entries than the table holds")
}

func TestWAV(t *testing.T) {
	t.Parallel()
	pcm := Sine(440, 8000, 100*time.Millisecond)
	assert.Len(t, pcm, 800*4)
	data, err := WAV(pcm, 8000, 2)
	require.NoError(t, err)
	assert.Len(t, data, wav.HeaderSize+len(pcm))
	assert.Equal(t, "RIFF", string(data[:4]))
	assert.Equal(t, uint32(8000), binary.LittleEndian.Uint32(data[24:28]))
	assert.Equal(t, pcm, data[wav.HeaderSize:])

	_, err = WAV(pcm, 0, 2)
	require.ErrorIs(t, err, wav.ErrHeader)

	assert.NotEqual(t, Sine(440, 8000, time.Millisecond*10), Sine(880, 8000, time.Millisecond*10))
}

func TestFilesAndEncoder(t *testing.T) {
	t.Parallel()
	path := SineWAV(t, 440)
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Positive(t, info.Size())

	var enc Encoder
	out := filepath.Join(t.TempDir(), "out.m4a")
	require.NoError(t, enc.Encode(t.Context(), path, out))
	first, err := os.ReadFile(out)
	require.NoError(t, err)
	_, _, err = MdatPayload(first)
	require.NoError(t, err)

	require.NoError(t, enc.Encode(t.Context(), SineWAV(t, 880), out))
	second, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.False(t, bytes.Equal(first, second), "different audio, different file")

	enc.Fail(errEncode)
	require.ErrorIs(t, enc.Encode(t.Context(), path, out), errEncode)
	enc.Fail(nil)
	require.Error(t, enc.Encode(t.Context(), filepath.Join(t.TempDir(), "missing.wav"), out))
	assert.Equal(t, 4, enc.Runs())

	calls := enc.Calls()
	require.Len(t, calls, 4)
	assert.Equal(t, EncodeCall{WAV: path, Codec: "aac", Rate: 44100, Channels: 2, Bits: 16}, calls[0])
	assert.Equal(t, EncodeCall{WAV: calls[3].WAV, Codec: "aac"}, calls[3], "a missing WAV has no header to report")
}

func TestEncoderCodec(t *testing.T) {
	t.Parallel()
	path := SineWAV(t, 440)
	dir := t.TempDir()
	encode := func(codec string) []byte {
		t.Helper()
		out := filepath.Join(dir, codec+".m4a")
		require.NoError(t, (&Encoder{Codec: codec}).Encode(t.Context(), path, out))
		b, err := os.ReadFile(out)
		require.NoError(t, err)
		return b
	}
	aac, alac := encode(""), encode("alac")
	entry := func(file []byte) string {
		t.Helper()
		stsd, err := find(file, "moov", "trak", "mdia", "minf", "stbl", "stsd")
		require.NoError(t, err)
		require.Len(t, stsd, 1)
		entries, err := boxes(stsd[0].payload[8:], stsd[0].off+8)
		require.NoError(t, err)
		require.Len(t, entries, 1)
		return entries[0].typ
	}
	assert.Equal(t, "mp4a", entry(aac), "the zero value encodes AAC")
	assert.Equal(t, "alac", entry(alac))
	a, _, err := MdatPayload(aac)
	require.NoError(t, err)
	b, _, err := MdatPayload(alac)
	require.NoError(t, err)
	assert.NotEqual(t, a, b, "one recording in two codecs is two different audio payloads")
}

func TestSampleEntries(t *testing.T) {
	t.Parallel()
	for codec, typ := range map[string]string{"aac": "mp4a", "alac": "alac", "ac-3": "ac-3"} {
		got, err := boxes(SampleEntry(codec), 0)
		require.NoError(t, err)
		require.Len(t, got, 1)
		assert.Equal(t, typ, got[0].typ)
	}
	stsd, err := find(M4A(M4AOptions{}), "moov", "trak", "mdia", "minf", "stbl", "stsd")
	require.NoError(t, err)
	require.Len(t, stsd, 1)
	assert.Equal(t, make([]byte, 8), stsd[0].payload, "no codec: an empty sample description")
}

func TestM4AFFormats(t *testing.T) {
	t.Parallel()
	help := "Audio file and data formats:\n" +
		"    'adts' = AAC ADTS (.aac, .adts)\n" +
		"               data_formats: 'aac ' 'aach' 'aacp' \n" +
		"    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n" +
		"               data_formats: '.mp1' 'aac ' 'aace' \n" +
		"                             'aacg' 'alac' \n" +
		"    'm4bf' = Apple MPEG-4 AudioBooks (.m4b)\n" +
		"               data_formats: 'flac' \n"
	got := m4afFormats(help)
	assert.Contains(t, got, "'alac'")
	assert.Contains(t, got, "'aac '")
	assert.NotContains(t, got, "'flac'", "another file type's list")
	sandboxed := m4afFormats("    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: \n" +
		"    'caff' = CAF (.caf, .caff)\n               data_formats: 'alaw' LEI16 \n")
	assert.NotContains(t, sandboxed, "'", "a sandbox lists no data format for M4A")
	assert.Equal(t, "aac ", fourCC("aac"))
	assert.Equal(t, "alac", fourCC("alac"))
}

func TestPtrAndMagic(t *testing.T) {
	t.Parallel()
	p := Ptr(42)
	*p++
	assert.Equal(t, 43, *p)
	assert.Equal(t, []byte{0xFF, 0xD8, 0xFF}, JPEG()[:3])
	assert.Equal(t, "\x89PNG", string(PNG()[:4]))
}

func TestRequireLiveSkipsUnlessAskedFor(t *testing.T) {
	t.Parallel()
	if os.Getenv(EnvLiveCapture) == "1" {
		t.Skip("live tests are enabled in this run")
	}
	ran := false
	t.Run("live", func(t *testing.T) {
		t.Parallel()
		RequireLive(t)
		ran = true
	})
	t.Cleanup(func() { assert.False(t, ran, "a live test never runs by default") })
}

func TestRealM4AIsTaggable(t *testing.T) {
	t.Parallel()
	for _, codec := range []string{"aac", "alac"} {
		t.Run(codec, func(t *testing.T) {
			t.Parallel()
			file := RealM4A(t, codec) // skips where the codec cannot be encoded
			payload, _, err := MdatPayload(file)
			require.NoError(t, err)
			assert.NotEmpty(t, payload)
			offsets, err := ChunkOffsets(file)
			require.NoError(t, err)
			assert.NotEmpty(t, offsets)
		})
	}
}
