package mp4tag

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestParseCodec(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]Codec{"alac": CodecALAC, " ALAC ": CodecALAC, "aac": CodecAAC, "AAC": CodecAAC} {
		got, err := ParseCodec(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "mp3", "flac", "mp4a"} {
		_, err := ParseCodec(in)
		require.ErrorIs(t, err, ErrUnknownCodec, in)
	}
	assert.True(t, CodecALAC.Lossless())
	assert.False(t, CodecAAC.Lossless())
}

// track builds a trak with the given handler type and sample entries.
func track(handler string, entries ...[]byte) []byte {
	stsd := testutil.FullBox("stsd", append([][]byte{binary.BigEndian.AppendUint32(nil, uint32(len(entries)))}, entries...)...)
	return testutil.Box("trak",
		testutil.FullBox("tkhd", make([]byte, 80)),
		testutil.Box("mdia",
			testutil.FullBox("mdhd", make([]byte, 20)),
			testutil.FullBox("hdlr", make([]byte, 4), []byte(handler), make([]byte, 13)),
			testutil.Box("minf", testutil.Box("stbl", stsd, testutil.FullBox("stco", make([]byte, 4)))),
		),
	)
}

func TestCodecOf(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		file []byte
		want Codec
		err  error
	}{
		"alac":                {file: testutil.M4A(testutil.M4AOptions{Codec: "alac"}), want: CodecALAC},
		"aac":                 {file: testutil.M4A(testutil.M4AOptions{Codec: "aac"}), want: CodecAAC},
		"aac, moov last":      {file: testutil.M4A(testutil.M4AOptions{Codec: "aac", MoovLast: true}), want: CodecAAC},
		"another codec":       {file: testutil.M4A(testutil.M4AOptions{Codec: "ac-3"}), want: Codec("ac-3")},
		"padded type trimmed": {file: withMoov(track("soun", testutil.SampleEntry("ac3 "))), want: Codec("ac3")},
		"blank type":          {file: withMoov(track("soun", testutil.SampleEntry("    "))), want: Codec("    ")},
		"no sample entry":     {file: testutil.M4A(testutil.M4AOptions{}), err: errNoSoundTrack},
		"no track":            {file: withMoov(), err: errNoSoundTrack},
		"no moov":             {file: testutil.Box("mdat", []byte("x")), err: errNoMoov},
		"sound track chosen": {
			file: withMoov(track("text", testutil.SampleEntry("tx3g")), track("soun", testutil.SampleEntry("alac"))),
			want: CodecALAC,
		},
		"first sample entry wins": {
			file: withMoov(track("soun", testutil.SampleEntry("aac"), testutil.SampleEntry("alac"))),
			want: CodecAAC,
		},
		"a silent sound track is skipped": {
			file: withMoov(track("soun"), track("soun", testutil.SampleEntry("alac"))),
			want: CodecALAC,
		},
		"no stsd": {
			file: withMoov(testutil.Box("trak", testutil.Box("mdia",
				testutil.FullBox("hdlr", make([]byte, 4), []byte("soun"), make([]byte, 13)),
				testutil.Box("minf", testutil.Box("stbl"))))),
			err: errNoSoundTrack,
		},
		"no hdlr":    {file: withMoov(testutil.Box("trak", testutil.Box("mdia"))), err: errNoSoundTrack},
		"short hdlr": {file: withMoov(testutil.Box("trak", testutil.Box("mdia", testutil.FullBox("hdlr")))), err: errNoSoundTrack},
		"short stsd": {
			file: withMoov(testutil.Box("trak", testutil.Box("mdia",
				testutil.FullBox("hdlr", make([]byte, 4), []byte("soun")),
				testutil.Box("minf", testutil.Box("stbl", testutil.Box("stsd")))))),
			err: errBadBox,
		},
		"broken trak":   {file: withMoov(testutil.Box("trak", overrun("mdia"))), err: errBadBox},
		"broken entry":  {file: withMoov(track("soun", overrun("alac"))), err: errBadBox},
		"broken moov":   {file: withMoov(overrun("trak")), err: errBadBox},
		"malformed top": {file: overrun("moov"), err: errBadBox},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := CodecOf(tc.file)
			path := testutil.WriteFile(t, "song.m4a", tc.file)
			fromFile, fileErr := ReadCodec(path)
			if tc.err != nil {
				require.ErrorIs(t, err, tc.err)
				require.ErrorIs(t, fileErr, tc.err, "ReadCodec agrees")
				return
			}
			require.NoError(t, err)
			require.NoError(t, fileErr)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.want, fromFile, "ReadCodec agrees")
		})
	}
}

// Tagging never changes what the file says its codec is.
func TestTagKeepsTheCodec(t *testing.T) {
	t.Parallel()
	for _, v := range variants() {
		if v.opts.Mdat == testutil.MdatToEnd && !v.opts.MoovLast {
			continue // the mdat swallows moov
		}
		o := v.opts
		o.Codec = "alac"
		out, err := Tag(testutil.M4A(o), fullTags())
		require.NoError(t, err, v.name)
		got, err := CodecOf(out)
		require.NoError(t, err, v.name)
		assert.Equal(t, CodecALAC, got, v.name)
	}
}

func TestReadCodecErrors(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	_, err := ReadCodec(filepath.Join(dir, "missing.m4a"))
	require.ErrorIs(t, err, os.ErrNotExist)
	_, err = ReadCodec(dir)
	require.Error(t, err, "a folder is not a file")
}

// FuzzCodecOf reads the codec of arbitrary bytes: never a panic, and the
// file reader agrees with the in-memory one wherever both succeed.
func FuzzCodecOf(f *testing.F) {
	addSeeds(f)
	f.Add(testutil.M4A(testutil.M4AOptions{Codec: "alac", Audio: []byte("x")}))
	f.Add(withMoov(track("text", testutil.SampleEntry("tx3g")), track("soun", testutil.SampleEntry("aac"))))
	path := filepath.Join(f.TempDir(), "fuzz.m4a")
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := CodecOf(data)
		require.NoError(t, os.WriteFile(path, data, 0o600))
		fromFile, fileErr := ReadCodec(path)
		if err == nil && fileErr == nil {
			assert.Equal(t, got, fromFile)
		}
		if err == nil {
			assert.NotEmpty(t, got, "a codec found has a name")
		}
	})
}
