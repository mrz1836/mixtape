package cli

import (
	"context"
	"maps"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestRecordFormatFlagsBecomeOverrides(t *testing.T) {
	t.Parallel()
	f := &recordFlags{}
	cmd := &cobra.Command{}
	f.bind(cmd.Flags())
	require.NoError(t, cmd.ParseFlags([]string{
		"--codec", "aac", "--aac-bitrate", "128", "--bit-depth", "24", "--copy-codec", "any", "--output-rate", "44100",
	}))
	o := f.overrides(cmd)
	assert.Equal(t, config.Overrides{
		Codec: testutil.Ptr("aac"), AACBitrate: testutil.Ptr(128), BitDepth: testutil.Ptr("24"),
		CopyCodec: testutil.Ptr("any"), OutputRate: testutil.Ptr("44100"),
	}, o)

	f, cmd = &recordFlags{}, &cobra.Command{}
	f.bind(cmd.Flags())
	require.NoError(t, cmd.ParseFlags(nil))
	assert.Equal(t, config.Overrides{}, f.overrides(cmd), "flags not given leave the config and env alone")
}

func TestChildFlagsForwardTheFormatFlags(t *testing.T) {
	t.Parallel()
	cmd := newRecordCmd(&app{})
	require.NoError(t, cmd.ParseFlags([]string{
		"--detach", "--codec", "aac", "--aac-bitrate", "320", "--bit-depth", "16", "--copy-codec", "same", "--output-rate", "keep",
	}))
	assert.ElementsMatch(t, []string{
		"--codec=aac", "--aac-bitrate=320", "--bit-depth=16", "--copy-codec=same", "--output-rate=keep",
	}, childFlags(cmd))
}

// The library encodes with the codec and bit rate the settings name, from
// any layer.
func TestRecordEncodesWithTheConfiguredCodec(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		args []string
		env  map[string]string
		want library.AFConvert
	}{
		"defaults: ALAC":      {want: library.AFConvert{Codec: mp4tag.CodecALAC, Bitrate: 256000}},
		"AAC at 320 by flags": {args: []string{"--codec", "aac", "--aac-bitrate", "320"}, want: library.AFConvert{Codec: mp4tag.CodecAAC, Bitrate: 320000}},
		"AAC by the env":      {env: map[string]string{config.EnvCodec: "aac", config.EnvAACBitrate: "128"}, want: library.AFConvert{Codec: mp4tag.CodecAAC, Bitrate: 128000}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			maps.Copy(h.env, tc.env)
			h.world.addPlaylist("List", songs()...)
			require.Equal(t, ExitOK, h.run(append([]string{"record", "--dry-run", plURL}, tc.args...)...), h.stderr.String())
			require.NotEmpty(t, h.world.encodedAs)
			got := h.world.encodedAs[len(h.world.encodedAs)-1]
			assert.Equal(t, tc.want.Codec, got.Codec)
			assert.Equal(t, tc.want.Bitrate, got.Bitrate)
			assert.NotNil(t, got.Warn, "a dropped bit rate is reported")
		})
	}
	h := newHarness(t)
	h.world.addPlaylist("List", songs()...)
	require.Equal(t, ExitUsage, h.run("record", "--dry-run", "--codec", "flac", plURL))
	assert.Contains(t, h.errDoc()["message"], `codec "flac" (want alac or aac)`)
}

// A song the library holds only in another codec is recorded again, with
// the reason in the plan and in list; copy_codec any copies it.
func TestCopyCodecDecidesThePlan(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("Mix", songs()...)
	h.seedLibraryAs(mp4tag.CodecAAC, library.Spec{Kind: library.KindSingles}, songs()[0])

	require.Equal(t, ExitOK, h.run("record", "--dry-run", plURL))
	items := h.json()["items"].([]any)
	first := items[0].(map[string]any)
	assert.Equal(t, "record", first["action"])
	assert.Equal(t, engine.ReasonOtherCodec, first["reason"])

	require.Equal(t, ExitOK, h.run("record", "--dry-run", "-o", "text", plURL))
	assert.Contains(t, h.stdout.String(), "(in the library in another codec)")

	require.Equal(t, ExitOK, h.run("list", plURL))
	row := trackRow(t, h.json(), trkA)
	assert.Equal(t, "record", row["next_action"], "list agrees with the plan")
	assert.Equal(t, engine.ReasonOtherCodec, row["reason"])

	for _, args := range [][]string{{"record", "--dry-run", "--copy-codec", "any", plURL}, {"record", "--dry-run", "--codec", "aac", plURL}} {
		require.Equal(t, ExitOK, h.run(args...))
		first = h.json()["items"].([]any)[0].(map[string]any)
		assert.Equal(t, "copy", first["action"], "%v", args)
		assert.Nil(t, first["reason"], "%v", args)
	}
	h.env[config.EnvCopyCodec] = "any"
	require.Equal(t, ExitOK, h.run("list", plURL))
	assert.Equal(t, "copy", trackRow(t, h.json(), trkA)["next_action"])
}

// Copies and the session start name the codec, in events and text.
func TestCopiesNameTheirCodec(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("Mix", songs()...)
	h.seedLibrary(library.Spec{Kind: library.KindSingles}, songs()...)
	require.Equal(t, ExitOK, h.run("record", plURL))
	var copied int
	for _, ev := range h.ndjson() {
		switch ev.Type {
		case engine.EventSessionStarted:
			assert.Equal(t, "alac", ev.Codec)
			assert.Zero(t, ev.BitDepth, "nothing plays: no capture to describe")
		case engine.EventTrackCopied:
			copied++
			assert.Equal(t, "alac", ev.Track.Codec)
		}
	}
	assert.Equal(t, 2, copied)
	assert.Equal(t, "alac", trackRow(t, listDocOf(t, h), trkA)["codec"])
}

// listDocOf runs list for the test playlist and returns its document.
func listDocOf(t *testing.T, h *harness) map[string]any {
	t.Helper()
	require.Equal(t, ExitOK, h.run("list", plURL))
	return h.json()
}

func TestEventLinesNameTheFormat(t *testing.T) {
	t.Parallel()
	o := plainOutput(t, false)
	started := eventLine(engine.Event{
		Type: engine.EventSessionStarted, Mode: "queue", Capture: "tap (process tap)", Library: "/lib",
		Codec: "alac", BitDepth: 24, SampleRate: 44100,
	}, o)
	assert.Contains(t, started, "saving ALAC, 24-bit 44.1 kHz")
	assert.NotContains(t, eventLine(engine.Event{Type: engine.EventSessionStarted, Mode: "queue"}, o), "saving")
	saved := eventLine(engine.Event{Type: engine.EventTrackSaved, Track: &engine.EventTrack{Title: "T", File: "Mix/T.m4a", Codec: "aac"}}, o)
	assert.Contains(t, saved, "Mix/T.m4a (AAC)")
	assert.Equal(t, "ALAC", formatLabel("alac", 0, 48000), "no depth: just the codec")
	assert.Equal(t, "48 kHz", kHz(48000))
}

func TestDoctorEncoderCheck(t *testing.T) {
	t.Parallel()
	const both = "    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: 'aac ' 'alac' \n"
	const aacOnly = "    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: 'aac ' \n"
	const sandboxed = "    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: \n"
	for name, tc := range map[string]struct {
		help, codec    string
		status, detail string
	}{
		"ALAC listed":           {help: both, status: checkOK, detail: "ALAC, lossless (24-bit capture)"},
		"AAC listed":            {help: both, codec: "aac", status: checkOK, detail: "AAC at 256 kb/s (16-bit capture)"},
		"ALAC missing":          {help: aacOnly, status: checkWarn, detail: "afconvert does not list the ALAC encoder here"},
		"a sandbox hides both":  {help: sandboxed, codec: "aac", status: checkWarn, detail: "afconvert does not list the AAC encoder here"},
		"another file type has": {help: "    'caff' = CAF\n               data_formats: 'alac' \n", status: checkWarn},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.deps.services.encoder = nil // the real encoder, whose listing is checked
			h.deps.services.afconvertHelp = func(context.Context) string { return tc.help }
			if tc.codec != "" {
				h.env[config.EnvCodec] = tc.codec
			}
			require.Equal(t, ExitOK, h.run("doctor"), "an encoder warning never blocks")
			c := findCheck(t, h.json(), checkIDEncoder)
			assert.Equal(t, tc.status, c["status"])
			if tc.detail != "" {
				assert.Equal(t, tc.detail, c["detail"])
			}
			if tc.status == checkWarn {
				assert.Equal(t, fixEncoder, c["fix"])
			}
		})
	}
}

func TestAfconvertCodecs(t *testing.T) {
	t.Parallel()
	help := "Audio file and data formats:\n" +
		"    'adts' = AAC ADTS (.aac, .adts)\n               data_formats: 'aac ' 'aach' \n" +
		"    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: '.mp1' 'aac ' 'aace' \n" +
		"                             'aacg' 'aach' 'alac' \n" +
		"    'm4bf' = Apple MPEG-4 AudioBooks (.m4b)\n               data_formats: 'aac ' \n"
	assert.Equal(t, map[mp4tag.Codec]bool{mp4tag.CodecAAC: true, mp4tag.CodecALAC: true}, afconvertCodecs(help))
	assert.Empty(t, afconvertCodecs("    'm4af' = Apple MPEG-4 Audio\n               data_formats: \n"))
	assert.Empty(t, afconvertCodecs(""))
}
