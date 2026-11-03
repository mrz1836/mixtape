package library

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
	"github.com/mrz1836/mixtape/internal/wav"
)

var errRejected = errors.New("afconvert rejected it")

// argsRecorder stands in for afconvert: it records each call's arguments,
// fails the calls fail says, and otherwise writes a file at the output path.
type argsRecorder struct {
	mu    sync.Mutex
	calls [][]string
	fail  func(call int) error
}

func (r *argsRecorder) run(_ context.Context, args ...string) error {
	r.mu.Lock()
	r.calls = append(r.calls, args)
	n := len(r.calls)
	r.mu.Unlock()
	if r.fail != nil {
		if err := r.fail(n); err != nil {
			return err
		}
	}
	return os.WriteFile(args[len(args)-1], []byte("m4a"), 0o600)
}

func TestAFConvertArgs(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		enc  AFConvert
		want []string
	}{
		"zero value: AAC at 256 kb/s": {AFConvert{}, []string{"-f", "m4af", "-d", "aac", "-b", "256000"}},
		"AAC at 128 kb/s":             {AFConvert{Codec: mp4tag.CodecAAC, Bitrate: 128000}, []string{"-f", "m4af", "-d", "aac", "-b", "128000"}},
		"AAC at 320 kb/s":             {AFConvert{Bitrate: 320000}, []string{"-f", "m4af", "-d", "aac", "-b", "320000"}},
		"ALAC has no bit rate":        {AFConvert{Codec: mp4tag.CodecALAC, Bitrate: 320000}, []string{"-f", "m4af", "-d", "alac"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := &argsRecorder{}
			tc.enc.run = rec.run
			out := filepath.Join(t.TempDir(), "out.m4a")
			require.NoError(t, tc.enc.Encode(t.Context(), "in.wav", out))
			require.Len(t, rec.calls, 1)
			assert.Equal(t, append(tc.want, "in.wav", out), rec.calls[0])
		})
	}
}

func TestAFConvertRetriesWithoutTheBitrateAndSaysSo(t *testing.T) {
	t.Parallel()
	var warned []string
	rec := &argsRecorder{fail: func(call int) error {
		if call == 1 {
			return errRejected
		}
		return nil
	}}
	enc := AFConvert{Bitrate: 320000, Warn: func(m string) { warned = append(warned, m) }, run: rec.run}
	out := filepath.Join(t.TempDir(), "out.m4a")
	require.NoError(t, enc.Encode(t.Context(), "/work/song.wav", out))
	require.Len(t, rec.calls, 2)
	assert.Equal(t, []string{"-f", "m4af", "-d", "aac", "/work/song.wav", out}, rec.calls[1], "the retry drops -b")
	require.Len(t, warned, 1)
	assert.Contains(t, warned[0], "320 kb/s")
	assert.Contains(t, warned[0], "song.wav")
	assert.Contains(t, warned[0], errRejected.Error())

	// No Warn: the retry still happens, silently.
	rec = &argsRecorder{fail: func(call int) error {
		if call == 1 {
			return errRejected
		}
		return nil
	}}
	require.NoError(t, AFConvert{run: rec.run}.Encode(t.Context(), "in.wav", out))
	assert.Len(t, rec.calls, 2)
}

func TestAFConvertFailures(t *testing.T) {
	t.Parallel()
	always := func(int) error { return errRejected }
	for name, enc := range map[string]AFConvert{"AAC": {}, "ALAC": {Codec: mp4tag.CodecALAC}} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out := filepath.Join(t.TempDir(), "out.m4a")
			require.NoError(t, os.WriteFile(out, []byte("half a file"), 0o600))
			rec := &argsRecorder{fail: always}
			enc.run = rec.run
			require.ErrorIs(t, enc.Encode(t.Context(), "in.wav", out), errRejected)
			assert.NoFileExists(t, out, "nothing half-written is left")
		})
	}

	// afconvert succeeds but leaves nothing: an error, not an empty song.
	enc := AFConvert{Codec: mp4tag.CodecALAC, run: func(context.Context, ...string) error { return nil }}
	require.Error(t, enc.Encode(t.Context(), "in.wav", filepath.Join(t.TempDir(), "out.m4a")))
}

func TestParseCopyPolicy(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]CopyPolicy{"same": CopySame, " SAME": CopySame, "any": CopyAny, "Any": CopyAny} {
		got, err := ParseCopyPolicy(in)
		require.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, in := range []string{"", "never", "all"} {
		_, err := ParseCopyPolicy(in)
		require.ErrorIs(t, err, ErrCopyPolicy, in)
	}
}

// fileCodec reads the codec of the song stored for e in c.
func fileCodec(t *testing.T, c *Collection, e Entry) mp4tag.Codec {
	t.Helper()
	got, err := mp4tag.ReadCodec(c.Path(&e))
	require.NoError(t, err)
	return got
}

func TestCommitRecordsTheCodec(t *testing.T) {
	t.Parallel()
	for _, codec := range []mp4tag.Codec{mp4tag.CodecALAC, mp4tag.CodecAAC} {
		t.Run(string(codec), func(t *testing.T) {
			t.Parallel()
			f := newCodecFixture(t, codec, CopySame)
			c := f.collection(playlistSpec())
			e := f.commit(c, trackTime(), 440, CommitOptions{})
			assert.Equal(t, codec, e.Codec)
			assert.Equal(t, codec, fileCodec(t, c, e), "the cache says what the file says")
			assert.Equal(t, codec, readManifest(t, c.Dir).Tracks[0].Codec, "the manifest records it")
		})
	}
}

// --redo with another codec replaces the file, and the entry follows it.
func TestRedoInAnotherCodecUpdatesTheEntry(t *testing.T) {
	t.Parallel()
	f := newCodecFixture(t, mp4tag.CodecAAC, CopySame)
	c := f.collection(playlistSpec())
	first := f.commit(c, trackTime(), 440, CommitOptions{})
	require.Equal(t, mp4tag.CodecAAC, first.Codec)

	f.as(mp4tag.CodecALAC, CopySame)
	c, ok := f.lib.Lookup(playlistSpec())
	require.True(t, ok)
	again := f.commit(c, trackTime(), 440, CommitOptions{})
	assert.Equal(t, first.File, again.File, "replaced in place")
	assert.Equal(t, mp4tag.CodecALAC, again.Codec)
	assert.Equal(t, mp4tag.CodecALAC, fileCodec(t, c, again))
	assert.NotEqual(t, first.AudioSHA256, again.AudioSHA256, "other audio, other hash")
}

func TestCopyKeepsTheCodec(t *testing.T) {
	t.Parallel()
	f := newCodecFixture(t, mp4tag.CodecALAC, CopySame)
	src := f.collection(playlistSpec())
	orig := f.commit(src, trackTime(), 440, CommitOptions{})

	singles := f.collection(Spec{Kind: KindSingles})
	locs, other := f.lib.FindCopy(idTime)
	require.Len(t, locs, 1)
	require.Empty(t, other)
	copied, err := f.lib.Copy(t.Context(), singles, trackTime(), locs[0])
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecALAC, copied.Codec)
	assert.Equal(t, mp4tag.CodecALAC, fileCodec(t, singles, copied))
	assert.Equal(t, orig.AudioSHA256, copied.AudioSHA256)
}

func TestFindCopy(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		recorded mp4tag.Codec // the song in the library
		wanted   mp4tag.Codec // what this run records
		policy   CopyPolicy
		copyable bool
	}{
		"same codec, same policy":    {mp4tag.CodecALAC, mp4tag.CodecALAC, CopySame, true},
		"other codec, same policy":   {mp4tag.CodecAAC, mp4tag.CodecALAC, CopySame, false},
		"lossless for an AAC run":    {mp4tag.CodecALAC, mp4tag.CodecAAC, CopySame, false},
		"other codec, any policy":    {mp4tag.CodecAAC, mp4tag.CodecALAC, CopyAny, true},
		"other codec, no policy":     {mp4tag.CodecAAC, mp4tag.CodecALAC, "", true},
		"no codec set means AAC":     {mp4tag.CodecAAC, "", CopySame, true},
		"no codec set refuses ALAC":  {mp4tag.CodecALAC, "", CopySame, false},
		"AAC for an AAC run, any":    {mp4tag.CodecAAC, mp4tag.CodecAAC, CopyAny, true},
		"ALAC for an ALAC run, any":  {mp4tag.CodecALAC, mp4tag.CodecALAC, CopyAny, true},
		"ALAC for an AAC run, any":   {mp4tag.CodecALAC, mp4tag.CodecAAC, CopyAny, true},
		"same codec, AAC, same rule": {mp4tag.CodecAAC, mp4tag.CodecAAC, CopySame, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newCodecFixture(t, tc.recorded, CopyAny)
			f.commit(f.collection(playlistSpec()), trackTime(), 440, CommitOptions{})
			lib := f.as(tc.wanted, tc.policy)
			locs, other := lib.FindCopy(idTime)
			if tc.copyable {
				assert.Len(t, locs, 1)
				assert.Empty(t, other)
			} else {
				assert.Empty(t, locs)
				assert.Len(t, other, 1, "the song is reported as found in another codec")
			}
			assert.Len(t, lib.Find(idTime), 1, "Find itself does not look at codecs")
		})
	}
	f := newCodecFixture(t, mp4tag.CodecALAC, CopySame)
	locs, other := f.lib.FindCopy(idMoney)
	assert.Empty(t, locs, "nothing recorded")
	assert.Empty(t, other)
}

// The file decides, not the manifest's cache: an entry that claims ALAC
// over an AAC file is not copied as ALAC.
func TestFindCopyReadsTheFile(t *testing.T) {
	t.Parallel()
	f := newCodecFixture(t, mp4tag.CodecAAC, CopySame)
	c := f.collection(playlistSpec())
	f.commit(c, trackTime(), 440, CommitOptions{})
	crash(t, c.Dir, func(m *Manifest) { m.Tracks[0].Codec = mp4tag.CodecALAC })

	lib := f.as(mp4tag.CodecALAC, CopySame)
	locs, other := lib.FindCopy(idTime)
	assert.Empty(t, locs)
	require.Len(t, other, 1)

	// A file whose codec cannot be read is never a same-codec copy.
	path := other[0].Path
	retag(t, path, mp4tag.Tags{Freeform: map[string]string{TagSpotifyID: idTime}})
	require.NoError(t, os.WriteFile(path, testutil.M4A(testutil.M4AOptions{}), 0o600))
	locs, _ = f.as(mp4tag.CodecAAC, CopySame).FindCopy(idTime)
	assert.Empty(t, locs, "no sample entry: no codec to match")
}

func TestReconcileAdoptsTheCodec(t *testing.T) {
	t.Parallel()
	f := newCodecFixture(t, mp4tag.CodecALAC, CopySame)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime()}, true)
	f.commit(c, trackTime(), 440, CommitOptions{})
	crash(t, c.Dir, func(m *Manifest) {
		forget(&m.Tracks[0])
		m.Tracks[0].Codec = ""
	})
	_, c2 := f.reopen(playlistSpec())
	require.True(t, c2.Done(idTime))
	assert.Equal(t, mp4tag.CodecALAC, c2.Entry(idTime).Codec, "the adopted file's codec")
}

func TestVerifyFixesKeepTheCodecCurrent(t *testing.T) {
	t.Parallel()
	f := newCodecFixture(t, mp4tag.CodecALAC, CopySame)
	c := f.collection(playlistSpec())
	c.Sync([]Track{trackTime(), trackMoney(), {ID: idRick, Title: "Never Gonna Give You Up"}}, true)
	timeEntry := f.commit(c, trackTime(), 440, CommitOptions{})
	f.commit(c, trackMoney(), 550, CommitOptions{})
	rick := f.commit(c, Track{ID: idRick, Title: "Never Gonna Give You Up"}, 660, CommitOptions{})

	// Time moved within its folder, Money's manifest hash and codec stale,
	// Rick's file gone.
	require.NoError(t, os.Rename(c.Path(&timeEntry), filepath.Join(c.Dir, "moved.m4a")))
	require.NoError(t, os.Remove(c.Path(&rick)))
	crash(t, c.Dir, func(m *Manifest) {
		for i := range m.Tracks {
			m.Tracks[i].Codec = ""
		}
		m.Tracks[1].AudioSHA256 = ""
	})
	lib, c2 := f.reopen(playlistSpec())
	_, err := lib.Verify(t.Context(), true, true)
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecALAC, c2.Entry(idTime).Codec, "moved: the found file's codec")
	assert.Equal(t, mp4tag.CodecALAC, c2.Entry(idMoney).Codec, "stale hash: refreshed with the codec")
	assert.Empty(t, c2.Entry(idRick).Codec, "missing: no file, no codec")

	// A status mismatch fix takes the codec too.
	crash(t, c.Dir, func(m *Manifest) {
		m.Tracks[0].Status, m.Tracks[0].Codec = StatusFailed, ""
	})
	lib, c2 = f.reopen(playlistSpec())
	_, err = lib.Verify(t.Context(), true, true)
	require.NoError(t, err)
	assert.Equal(t, StatusRecorded, c2.Entry(idTime).Status)
	assert.Equal(t, mp4tag.CodecALAC, c2.Entry(idTime).Codec)
}

// fileEncoder "encodes" by writing a file built beforehand, so a test can
// measure what storing it costs without the cost of building it.
type fileEncoder struct{ file []byte }

func (e fileEncoder) Encode(_ context.Context, _, m4aPath string) error {
	return os.WriteFile(m4aPath, e.file, 0o600)
}

// Commit and Copy stream the song: storing a long recording costs a moov's
// worth of memory, not the file's.
func TestCommitAndCopyStream(t *testing.T) { //nolint:paralleltest // measures allocations, so it runs alone
	const size = 48 << 20
	f := newFixture(t, fileEncoder{file: testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0x5A}, size), Codec: "alac"})})
	c := f.collection(playlistSpec())
	wavPath := testutil.SineWAV(t, 440)

	var e Entry
	allocated := allocatedBy(func() {
		var err error
		e, err = f.lib.Commit(t.Context(), c, trackTime(), wavPath, CommitOptions{})
		require.NoError(t, err)
	})
	info, err := os.Stat(c.Path(&e))
	require.NoError(t, err)
	require.Greater(t, info.Size(), int64(size))
	assert.Less(t, allocated, uint64(size/4), "Commit of a %d MiB song allocated %d MiB", size>>20, allocated>>20)

	singles := f.collection(Spec{Kind: KindSingles})
	locs, _ := f.lib.FindCopy(idTime)
	require.Len(t, locs, 1)
	allocated = allocatedBy(func() {
		_, err = f.lib.Copy(t.Context(), singles, trackTime(), locs[0])
		require.NoError(t, err)
	})
	assert.Less(t, allocated, uint64(size/4), "Copy allocated %d MiB", allocated>>20)
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

// noise is d seconds of deterministic pseudo-random PCM16 stereo: audio no
// encoder can shrink much, and a lossy one cannot keep exactly.
func noise(rate int, seconds float64) []byte {
	n := int(seconds*float64(rate)) * 2
	pcm := make([]byte, 0, n*2)
	x := uint32(2463534242)
	for range n {
		x ^= x << 13
		x ^= x >> 17
		x ^= x << 5
		v := int16(x>>16) / 4
		pcm = append(pcm, byte(v), byte(v>>8))
	}
	return pcm
}

// decodePCM decodes the M4A at path back to 16-bit PCM with afconvert.
func decodePCM(t *testing.T, path string) []byte {
	t.Helper()
	out := filepath.Join(t.TempDir(), "decoded.wav")
	cmd := exec.CommandContext(t.Context(), "afconvert", path, out, "-f", "WAVE", "-d", "LEI16")
	b, err := cmd.CombinedOutput()
	require.NoError(t, err, string(b))
	data, err := os.ReadFile(filepath.Clean(out))
	require.NoError(t, err)
	return wavData(t, data)
}

// wavData returns the data chunk of a WAV file.
func wavData(t *testing.T, file []byte) []byte {
	t.Helper()
	for i := 12; i+8 <= len(file); {
		size := int(uint32(file[i+4]) | uint32(file[i+5])<<8 | uint32(file[i+6])<<16 | uint32(file[i+7])<<24)
		if string(file[i:i+4]) == "data" {
			require.LessOrEqual(t, i+8+size, len(file))
			return file[i+8 : i+8+size]
		}
		i += 8 + size + size&1
	}
	t.Fatal("no data chunk")
	return nil
}

// TestALACIsLossless stores a real ALAC recording end to end: tagged,
// placed and hashed, it still decodes to exactly the PCM that was
// captured.
func TestALACIsLossless(t *testing.T) {
	t.Parallel()
	testutil.RequireEncoder(t, "alac")
	f := newFixture(t, AFConvert{Codec: mp4tag.CodecALAC})
	f.codec = mp4tag.CodecALAC
	c := f.collection(playlistSpec())
	pcm := noise(44100, 0.5)
	data, err := testutil.WAV(pcm, 44100, 2)
	require.NoError(t, err)
	e, err := f.lib.Commit(t.Context(), c, trackTime(), testutil.WriteFile(t, "rec.wav", data), CommitOptions{})
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecALAC, e.Codec)
	assert.Equal(t, pcm, decodePCM(t, c.Path(&e)), "every sample survives encoding and tagging")

	hash, err := mp4tag.HashFile(c.Path(&e))
	require.NoError(t, err)
	assert.Equal(t, e.AudioSHA256, hash)
	info, err := exec.CommandContext(t.Context(), "afinfo", c.Path(&e)).CombinedOutput()
	require.NoError(t, err, string(info))
	assert.Contains(t, string(info), "from 16-bit source")
	assert.Len(t, data, wav.HeaderSize+len(pcm))
}

// TestAFConvertAACBitrate checks that the bit rate reaches the encoder.
func TestAFConvertAACBitrate(t *testing.T) {
	t.Parallel()
	testutil.RequireEncoder(t, "aac")
	data, err := testutil.WAV(noise(48000, 3), 48000, 2)
	require.NoError(t, err)
	in := testutil.WriteFile(t, "noise.wav", data)
	rate := regexp.MustCompile(`bit rate: (\d+) bits per second`)
	for _, kbps := range []int{128, 320} {
		out := filepath.Join(t.TempDir(), "out.m4a")
		require.NoError(t, AFConvert{Bitrate: kbps * 1000}.Encode(t.Context(), in, out))
		info, err := exec.CommandContext(t.Context(), "afinfo", out).CombinedOutput()
		require.NoError(t, err, string(info))
		m := rate.FindStringSubmatch(string(info))
		require.Len(t, m, 2, string(info))
		got, err := strconv.Atoi(m[1])
		require.NoError(t, err)
		assert.InDelta(t, kbps*1000, got, float64(kbps*1000)/10, "AAC at %d kb/s", kbps)
		assert.Contains(t, string(info), "48000 Hz", "the rate is kept at %d kb/s", kbps)
	}
}
