package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/testutil"
)

const testHome = "/Users/test"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveDefaults(t *testing.T) {
	t.Parallel()
	s, err := Resolve(File{}, env(nil), Overrides{}, testHome)
	require.NoError(t, err)

	assert.Equal(t, "/Users/test/Music/Mixtape", s.Library)
	assert.Equal(t, capture.MethodAuto, s.Capture)
	assert.Equal(t, DefaultDevice, s.Device)
	assert.False(t, s.Artwork)
	assert.Equal(t, 10*time.Minute, s.IdleTimeout)
	for _, k := range Keys() {
		assert.Equal(t, SourceDefault, s.Sources[k.Name], k.Name)
	}
}

func TestResolvePrecedence(t *testing.T) {
	t.Parallel()
	f := File{
		Library:     testutil.Ptr("~/Archive"),
		Capture:     testutil.Ptr("device"),
		Artwork:     testutil.Ptr(true),
		IdleTimeout: testutil.Ptr("5m"),
	}
	e := env(map[string]string{EnvCapture: "TAP", EnvDevice: "Loopback"})
	o := Overrides{Library: testutil.Ptr("/Volumes/Ext/Music"), IdleTimeout: testutil.Ptr(time.Duration(0))}

	s, err := Resolve(f, e, o, testHome)
	require.NoError(t, err)

	assert.Equal(t, "/Volumes/Ext/Music", s.Library)
	assert.Equal(t, SourceFlag, s.Sources[KeyLibrary])
	assert.Equal(t, capture.MethodTap, s.Capture, "env beats file, and values are case-folded")
	assert.Equal(t, SourceEnv, s.Sources[KeyCapture])
	assert.Equal(t, "Loopback", s.Device)
	assert.True(t, s.Artwork)
	assert.Equal(t, SourceFile, s.Sources[KeyArtwork])
	assert.Equal(t, time.Duration(0), s.IdleTimeout)
	assert.Equal(t, SourceFlag, s.Sources[KeyIdleTimeout])
}

func TestResolveInvalid(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		file File
		env  map[string]string
	}{
		"bad capture in file":   {file: File{Capture: testutil.Ptr("blackhole")}},
		"bad duration in env":   {env: map[string]string{EnvIdleTimeout: "soon"}},
		"negative duration":     {env: map[string]string{EnvIdleTimeout: "-1m"}},
		"bad bool in env":       {env: map[string]string{EnvArtwork: "maybe"}},
		"empty library in file": {file: File{Library: testutil.Ptr("  ")}},
		"empty device in file":  {file: File{Device: testutil.Ptr("")}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := Resolve(tc.file, env(tc.env), Overrides{}, testHome)
			require.ErrorIs(t, err, ErrInvalidValue)
		})
	}
}

func TestLoad(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	f, warnings, err := Load(filepath.Join(dir, "missing.json"))
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, File{}, f)

	path := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"capture":"tap","future_key":1,"artwork":true}`), 0o600))
	f, warnings, err = Load(path)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	assert.Contains(t, warnings[0], `"future_key"`)
	assert.Equal(t, "tap", *f.Capture)
	assert.True(t, *f.Artwork)

	require.NoError(t, os.WriteFile(path, []byte(`{not json`), 0o600))
	_, _, err = Load(path)
	require.ErrorIs(t, err, ErrDecode)

	require.NoError(t, os.WriteFile(path, []byte(`{"artwork":"yes"}`), 0o600))
	_, _, err = Load(path)
	require.ErrorIs(t, err, ErrDecode, "a value of the wrong JSON type is a decode error")
}

func TestSetRoundTrip(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "home", "config.json")

	require.NoError(t, Set(path, KeyCapture, "Device", testHome))
	require.NoError(t, Set(path, KeyArtwork, "true", testHome))
	require.NoError(t, Set(path, KeyLibrary, "~/Tapes", testHome))

	f, warnings, err := Load(path)
	require.NoError(t, err)
	assert.Empty(t, warnings)
	assert.Equal(t, "device", *f.Capture)
	assert.True(t, *f.Artwork)
	assert.Equal(t, "~/Tapes", *f.Library, "the file keeps the portable ~/ form")

	s, err := Resolve(f, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, "/Users/test/Tapes", s.Library)

	require.NoError(t, Set(path, KeyCapture, "", testHome))
	f, _, err = Load(path)
	require.NoError(t, err)
	assert.Nil(t, f.Capture, "an empty value removes the key")

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestMaxFailures(t *testing.T) {
	t.Parallel()
	s, err := Resolve(File{}, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, DefaultMaxFailures, s.MaxFailures)
	assert.Equal(t, "3", s.Value(KeyMaxFailures))

	s, err = Resolve(File{MaxFailures: testutil.Ptr(5)}, env(map[string]string{EnvMaxFailures: "2"}), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, 2, s.MaxFailures, "env beats file")
	s, err = Resolve(File{MaxFailures: testutil.Ptr(5)}, env(nil), Overrides{MaxFailures: testutil.Ptr(0)}, testHome)
	require.NoError(t, err)
	assert.Equal(t, 0, s.MaxFailures, "a flag beats both; 0 means never")
	assert.Equal(t, SourceFlag, s.Sources[KeyMaxFailures])

	for _, bad := range []string{"-1", "lots", "1.5"} {
		_, badErr := Resolve(File{}, env(map[string]string{EnvMaxFailures: bad}), Overrides{}, testHome)
		require.ErrorIs(t, badErr, ErrInvalidValue, bad)
	}

	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, Set(path, KeyMaxFailures, "4", testHome))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"max_failures": 4`, "stored as a number")
	f, _, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 4, *f.MaxFailures)
}

func TestSetKeepsUnknownKeys(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"future_key":{"a":1}}`), 0o600))

	require.NoError(t, Set(path, KeyDevice, "Loopback", testHome))

	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"future_key"`)
	assert.Contains(t, string(data), `"device": "Loopback"`)
}

func TestSetRejects(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	require.ErrorIs(t, Set(path, "volume", "11", testHome), ErrUnknownKey)
	require.ErrorIs(t, Set(path, KeyCapture, "magic", testHome), ErrInvalidValue)
	require.ErrorIs(t, Set(path, KeyIdleTimeout, "forever", testHome), ErrInvalidValue)
	_, err := os.Stat(path)
	require.ErrorIs(t, err, os.ErrNotExist, "a rejected set writes nothing")
}

func TestSettingsValue(t *testing.T) {
	t.Parallel()
	s, err := Resolve(File{}, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, "false", s.Value(KeyArtwork))
	assert.Equal(t, "10m0s", s.Value(KeyIdleTimeout))
	assert.Equal(t, "auto", s.Value(KeyCapture))
	assert.Empty(t, s.Value("nope"))
}

func TestExpandHome(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/Users/test", ExpandHome("~", testHome))
	assert.Equal(t, "/Users/test/x", ExpandHome("~/x/", testHome))
	assert.Equal(t, "/abs/path", ExpandHome("/abs//path", testHome))
	wd, err := os.Getwd()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(wd, "rel"), ExpandHome("./rel", testHome), "relative paths become absolute")
}

func TestPaths(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/Users/test/.mixtape", DefaultHome(testHome))
	assert.Equal(t, "/h/config.json", Path("/h"))
}

func TestLoadIgnoresKeysThatDifferInCase(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"LIBRARY": "/elsewhere", "Device": "Loopback"}`), 0o600))
	f, warnings, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, File{}, f, "a key the warning calls ignored is ignored")
	assert.Len(t, warnings, 2)
}

func TestLoadNamesTheKeyOfAWrongType(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"artwork": "yes"}`), 0o600))
	_, _, err := Load(path)
	require.ErrorIs(t, err, ErrDecode)
	assert.Contains(t, err.Error(), "artwork")
}

func TestLosslessDefaults(t *testing.T) {
	t.Parallel()
	s, err := Resolve(File{}, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecALAC, s.Codec)
	assert.Equal(t, 256, s.AACBitrate)
	assert.Zero(t, s.BitDepth, "auto")
	assert.Equal(t, 24, s.Bits(), "auto records ALAC at 24 bits")
	assert.Equal(t, library.CopySame, s.CopyCodec)
	assert.Zero(t, s.OutputRate, "keep")
	for key, want := range map[string]string{
		KeyCodec: "alac", KeyAACBitrate: "256", KeyBitDepth: "auto", KeyCopyCodec: "same", KeyOutputRate: "keep",
	} {
		assert.Equal(t, want, s.Value(key), key)
	}
}

func TestSettingsBits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		codec mp4tag.Codec
		depth int
		want  int
	}{
		{mp4tag.CodecALAC, 0, 24},
		{mp4tag.CodecAAC, 0, 16},
		{mp4tag.CodecALAC, 16, 16},
		{mp4tag.CodecAAC, 24, 24},
		{"", 0, 16},
	} {
		assert.Equal(t, tc.want, Settings{Codec: tc.codec, BitDepth: tc.depth}.Bits(), "%s at %d", tc.codec, tc.depth)
	}
}

func TestLosslessPrecedence(t *testing.T) {
	t.Parallel()
	f := File{
		Codec: testutil.Ptr("aac"), AACBitrate: testutil.Ptr(128), BitDepth: testutil.Ptr(Text("16")),
		CopyCodec: testutil.Ptr("any"), OutputRate: testutil.Ptr(Text("48000")),
	}
	s, err := Resolve(f, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, Settings{
		Library: s.Library, Capture: s.Capture, Device: s.Device, IdleTimeout: s.IdleTimeout, MaxFailures: s.MaxFailures,
		Codec: mp4tag.CodecAAC, AACBitrate: 128, BitDepth: 16, CopyCodec: library.CopyAny, OutputRate: 48000,
		Sources: s.Sources,
	}, s)
	for _, k := range []string{KeyCodec, KeyAACBitrate, KeyBitDepth, KeyCopyCodec, KeyOutputRate} {
		assert.Equal(t, SourceFile, s.Sources[k], k)
	}

	e := env(map[string]string{
		EnvCodec: "ALAC", EnvAACBitrate: "320", EnvBitDepth: "auto", EnvCopyCodec: "SAME", EnvOutputRate: "44100",
	})
	s, err = Resolve(f, e, Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecALAC, s.Codec, "env beats file, case-folded")
	assert.Equal(t, 320, s.AACBitrate)
	assert.Zero(t, s.BitDepth, "auto from the env overrides 16 from the file")
	assert.Equal(t, library.CopySame, s.CopyCodec)
	assert.Equal(t, 44100, s.OutputRate)
	assert.Equal(t, SourceEnv, s.Sources[KeyOutputRate])

	o := Overrides{
		Codec: testutil.Ptr("aac"), AACBitrate: testutil.Ptr(64), BitDepth: testutil.Ptr("24"),
		CopyCodec: testutil.Ptr("any"), OutputRate: testutil.Ptr("keep"),
	}
	s, err = Resolve(f, e, o, testHome)
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecAAC, s.Codec, "a flag beats both")
	assert.Equal(t, 64, s.AACBitrate)
	assert.Equal(t, 24, s.Bits(), "24 bits even for AAC, when asked")
	assert.Equal(t, library.CopyAny, s.CopyCodec)
	assert.Zero(t, s.OutputRate)
	assert.Equal(t, SourceFlag, s.Sources[KeyOutputRate])
}

func TestLosslessInvalidValues(t *testing.T) {
	t.Parallel()
	for envName, values := range map[string][]string{
		EnvCodec:      {"mp3", "flac", "lossless"},
		EnvAACBitrate: {"63", "321", "256k", "-1", "0"},
		EnvBitDepth:   {"20", "32", "8", "auto-ish"},
		EnvCopyCodec:  {"some", "never"},
		EnvOutputRate: {"0", "7999", "768001", "44.1k", "44100.0"},
	} {
		for _, v := range values {
			_, err := Resolve(File{}, env(map[string]string{envName: v}), Overrides{}, testHome)
			require.ErrorIs(t, err, ErrInvalidValue, "%s=%q", envName, v)
			assert.Contains(t, err.Error(), "(from env)", "%s=%q", envName, v)
		}
	}
}

// bit_depth and output_rate mix words and numbers: config.json may hold
// either, and Set writes numbers as numbers.
func TestNumberOrStringSettings(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	load := func(body string) (File, error) {
		path := filepath.Join(dir, "config.json")
		require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
		f, _, err := Load(path)
		return f, err
	}
	for body, want := range map[string][2]int{
		`{"bit_depth": 24, "output_rate": 44100}`:       {24, 44100},
		`{"bit_depth": "16", "output_rate": "48000"}`:   {16, 48000},
		`{"bit_depth": "auto", "output_rate": "keep"}`:  {0, 0},
		`{"bit_depth": "AUTO", "output_rate": " KEEP"}`: {0, 0},
	} {
		f, err := load(body)
		require.NoError(t, err, body)
		s, err := Resolve(f, env(nil), Overrides{}, testHome)
		require.NoError(t, err, body)
		assert.Equal(t, want, [2]int{s.BitDepth, s.OutputRate}, body)
	}
	for _, body := range []string{`{"bit_depth": true}`, `{"output_rate": [44100]}`, `{"output_rate": {"hz": 1}}`} {
		_, err := load(body)
		require.ErrorIs(t, err, ErrDecode, body)
		var typeErr *json.UnmarshalTypeError
		require.ErrorAs(t, err, &typeErr, "a value of the wrong type, as encoding/json reports it: %s", body)
	}

	path := filepath.Join(dir, "set.json")
	for key, tc := range map[string]struct{ value, stored string }{
		KeyBitDepth:   {"24", `"bit_depth": 24`},
		KeyOutputRate: {"44100", `"output_rate": 44100`},
		KeyCodec:      {"AAC", `"codec": "aac"`},
		KeyCopyCodec:  {"Any", `"copy_codec": "any"`},
		KeyAACBitrate: {"320", `"aac_bitrate": 320`},
	} {
		require.NoError(t, Set(path, key, tc.value, testHome), key)
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Contains(t, string(data), tc.stored, key)
	}
	require.NoError(t, Set(path, KeyBitDepth, "auto", testHome))
	require.NoError(t, Set(path, KeyOutputRate, "Keep", testHome))
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Contains(t, string(data), `"bit_depth": "auto"`)
	assert.Contains(t, string(data), `"output_rate": "keep"`)
	f, _, err := Load(path)
	require.NoError(t, err)
	s, err := Resolve(f, env(nil), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, mp4tag.CodecAAC, s.Codec)
	assert.Equal(t, 16, s.Bits(), "auto again, for AAC")
	require.ErrorIs(t, Set(path, KeyBitDepth, "20", testHome), ErrInvalidValue)
	require.ErrorIs(t, Set(path, KeyCodec, "mp3", testHome), ErrInvalidValue)
}

func TestTextUnmarshal(t *testing.T) {
	t.Parallel()
	var v Text
	require.NoError(t, json.Unmarshal([]byte(`44100`), &v))
	assert.Equal(t, Text("44100"), v)
	require.NoError(t, json.Unmarshal([]byte(`"keep"`), &v))
	assert.Equal(t, Text("keep"), v)
	for raw, kind := range map[string]string{`true`: "bool", `{}`: "object", `[1]`: "array"} {
		var typeErr *json.UnmarshalTypeError
		require.ErrorAs(t, v.UnmarshalJSON([]byte(raw)), &typeErr, raw)
		assert.Equal(t, kind, typeErr.Value, raw)
	}
	assert.Equal(t, "nothing", jsonKind(nil))
	assert.Equal(t, "value", jsonKind([]byte("nul")))
}

func TestEverySettingIsDescribedOnce(t *testing.T) {
	t.Parallel()
	names := map[string]bool{}
	envs := map[string]bool{}
	for _, st := range table() {
		assert.False(t, names[st.Name], st.Name)
		assert.False(t, envs[st.Env], st.Env)
		names[st.Name], envs[st.Env] = true, true
		assert.NotEmpty(t, st.Help, st.Name)
		var defaults Settings
		require.NoError(t, st.apply(&defaults, st.Default, testHome), "the documented default of %s parses", st.Name)
		var f File
		var o Overrides
		_, inFile := fieldText(st.inFile(&f))
		_, inFlag := fieldText(st.inFlag(&o))
		assert.False(t, inFile || inFlag, "a zero File and Overrides set nothing")
	}
	assert.Len(t, Keys(), len(names))
}
