package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testHome = "/Users/test"

func env(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func ptr[T any](v T) *T { return &v }

func TestResolveDefaults(t *testing.T) {
	t.Parallel()
	s, err := Resolve(File{}, env(nil), Overrides{}, testHome)
	require.NoError(t, err)

	assert.Equal(t, "/Users/test/Music/Mixtape", s.Library)
	assert.Equal(t, CaptureAuto, s.Capture)
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
		Library:     ptr("~/Archive"),
		Capture:     ptr("device"),
		Artwork:     ptr(true),
		IdleTimeout: ptr("5m"),
	}
	e := env(map[string]string{EnvCapture: "TAP", EnvDevice: "Loopback"})
	o := Overrides{Library: ptr("/Volumes/Ext/Music"), IdleTimeout: ptr(time.Duration(0))}

	s, err := Resolve(f, e, o, testHome)
	require.NoError(t, err)

	assert.Equal(t, "/Volumes/Ext/Music", s.Library)
	assert.Equal(t, SourceFlag, s.Sources[KeyLibrary])
	assert.Equal(t, CaptureTap, s.Capture, "env beats file, and values are case-folded")
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
		"bad capture in file":   {file: File{Capture: ptr("blackhole")}},
		"bad duration in env":   {env: map[string]string{EnvIdleTimeout: "soon"}},
		"negative duration":     {env: map[string]string{EnvIdleTimeout: "-1m"}},
		"bad bool in env":       {env: map[string]string{EnvArtwork: "maybe"}},
		"empty library in file": {file: File{Library: ptr("  ")}},
		"empty device in file":  {file: File{Device: ptr("")}},
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

	s, err = Resolve(File{MaxFailures: ptr(5)}, env(map[string]string{EnvMaxFailures: "2"}), Overrides{}, testHome)
	require.NoError(t, err)
	assert.Equal(t, 2, s.MaxFailures, "env beats file")
	s, err = Resolve(File{MaxFailures: ptr(5)}, env(nil), Overrides{MaxFailures: ptr(0)}, testHome)
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
	assert.Equal(t, CaptureAuto, s.Value(KeyCapture))
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
