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
	file := RealM4A(t) // skips where AAC cannot be encoded
	payload, _, err := MdatPayload(file)
	require.NoError(t, err)
	assert.NotEmpty(t, payload)
	offsets, err := ChunkOffsets(file)
	require.NoError(t, err)
	assert.NotEmpty(t, offsets)
}
