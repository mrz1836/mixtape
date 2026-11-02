// Package config resolves Mixtape's settings from four layers, highest
// precedence first: command-line flags, MIXTAPE_* environment variables,
// the JSON config file (~/.mixtape/config.json), and built-in defaults.
//
// The file is plain JSON so it needs nothing beyond the standard library.
// Unknown keys are reported as warnings, never errors, so a config written
// by a newer Mixtape still loads in an older one.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mrz1836/mixtape/internal/fileutil"
)

// Setting names, as written in config.json and accepted by `config set`.
const (
	KeyLibrary     = "library"
	KeyCapture     = "capture"
	KeyDevice      = "device"
	KeyArtwork     = "artwork"
	KeyIdleTimeout = "idle_timeout"
)

// Environment variables.
const (
	EnvHome        = "MIXTAPE_HOME"
	EnvLibrary     = "MIXTAPE_LIBRARY"
	EnvCapture     = "MIXTAPE_CAPTURE"
	EnvDevice      = "MIXTAPE_DEVICE"
	EnvArtwork     = "MIXTAPE_ARTWORK"
	EnvIdleTimeout = "MIXTAPE_IDLE_TIMEOUT"
)

// Capture methods accepted by the capture setting.
const (
	CaptureAuto   = "auto"
	CaptureTap    = "tap"
	CaptureDevice = "device"
)

// Defaults.
const (
	DefaultHomeDir     = ".mixtape"
	DefaultLibraryDir  = "Music/Mixtape"
	DefaultDevice      = "BlackHole"
	DefaultIdleTimeout = 10 * time.Minute
	fileName           = "config.json"
)

var (
	// ErrUnknownKey reports a `config set` key Mixtape does not know.
	ErrUnknownKey = errors.New("unknown config key")
	// ErrInvalidValue reports a setting whose value fails validation.
	ErrInvalidValue = errors.New("invalid config value")
	// ErrDecode reports a config file that is not valid JSON.
	ErrDecode = errors.New("config file is not valid JSON")
)

// Source says which layer a resolved setting came from.
type Source string

// Setting sources, lowest precedence first.
const (
	SourceDefault Source = "default"
	SourceFile    Source = "file"
	SourceEnv     Source = "env"
	SourceFlag    Source = "flag"
)

// File is the on-disk shape of config.json. Every field is optional; a nil
// field falls through to the environment and then the default.
type File struct {
	Library     *string `json:"library,omitempty"`
	Capture     *string `json:"capture,omitempty"`
	Device      *string `json:"device,omitempty"`
	Artwork     *bool   `json:"artwork,omitempty"`
	IdleTimeout *string `json:"idle_timeout,omitempty"`
}

// Overrides carries the values of command-line flags that were set. A nil
// field means the flag was not given.
type Overrides struct {
	Library     *string
	Capture     *string
	Device      *string
	Artwork     *bool
	IdleTimeout *time.Duration
}

// Settings is the resolved configuration.
type Settings struct {
	Library     string
	Capture     string
	Device      string
	Artwork     bool
	IdleTimeout time.Duration
	// Sources maps each setting name to the layer that supplied it.
	Sources map[string]Source
}

// Key describes one setting for `config show`, `config set` and the docs.
type Key struct {
	Name    string
	Env     string
	Default string
	Help    string
}

// Keys lists every setting in display order.
func Keys() []Key {
	return []Key{
		{KeyLibrary, EnvLibrary, "~/" + DefaultLibraryDir, "Folder that holds every recording and manifest"},
		{KeyCapture, EnvCapture, CaptureAuto, "Capture method: auto, tap (Core Audio process tap) or device (BlackHole)"},
		{KeyDevice, EnvDevice, DefaultDevice, "Capture device name (substring) for the device method"},
		{KeyArtwork, EnvArtwork, "false", "Embed album artwork in each file (one image download per song)"},
		{KeyIdleTimeout, EnvIdleTimeout, DefaultIdleTimeout.String(), "End a session after this long with nothing recording (0 = never)"},
	}
}

// DefaultHome returns ~/.mixtape for the given user home directory.
func DefaultHome(userHome string) string { return filepath.Join(userHome, DefaultHomeDir) }

// Path returns the config file path inside a Mixtape home directory.
func Path(home string) string { return filepath.Join(home, fileName) }

// Load reads the config file at path. A missing file is not an error: it
// yields an empty File. Unknown keys come back as warnings.
func Load(path string) (File, []string, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if errors.Is(err, fs.ErrNotExist) {
		return File{}, nil, nil
	}
	if err != nil {
		return File{}, nil, err
	}
	raw, err := decodeRaw(data)
	if err != nil {
		return File{}, nil, err
	}
	var warnings []string
	for _, k := range sortedKeys(raw) {
		if !isKnown(k) {
			warnings = append(warnings, fmt.Sprintf("%s: ignoring unknown key %q", path, k))
		}
	}
	var f File
	if err := json.Unmarshal(data, &f); err != nil {
		return File{}, warnings, fmt.Errorf("%w: %s: %w", ErrDecode, path, err)
	}
	return f, warnings, nil
}

// Resolve layers the environment and flag overrides over the file and the
// defaults, validates the result, and expands a leading ~/ in the library
// path against userHome.
func Resolve(f File, getenv func(string) string, o Overrides, userHome string) (Settings, error) {
	s := Settings{
		Library:     filepath.Join(userHome, DefaultLibraryDir),
		Capture:     CaptureAuto,
		Device:      DefaultDevice,
		IdleTimeout: DefaultIdleTimeout,
		Sources:     map[string]Source{},
	}
	for _, k := range Keys() {
		s.Sources[k.Name] = SourceDefault
	}

	layers := []struct {
		src    Source
		values map[string]string
	}{
		{SourceFile, fileValues(f)},
		{SourceEnv, envValues(getenv)},
		{SourceFlag, flagValues(o)},
	}
	for _, layer := range layers {
		for _, k := range Keys() {
			v, ok := layer.values[k.Name]
			if !ok {
				continue
			}
			if err := applySetting(&s, k.Name, v, userHome); err != nil {
				return Settings{}, fmt.Errorf("%w (from %s)", err, layer.src)
			}
			s.Sources[k.Name] = layer.src
		}
	}
	return s, nil
}

// Value returns a setting rendered as text, for `config show`.
func (s Settings) Value(key string) string {
	switch key {
	case KeyLibrary:
		return s.Library
	case KeyCapture:
		return s.Capture
	case KeyDevice:
		return s.Device
	case KeyArtwork:
		return strconv.FormatBool(s.Artwork)
	case KeyIdleTimeout:
		return s.IdleTimeout.String()
	}
	return ""
}

// Set validates value for key and writes it to the config file at path,
// creating the file (and its directory) if needed. An empty value removes
// the key, restoring the default. Keys Mixtape does not know are kept as
// they are, so a downgrade never loses settings.
func Set(path, key, value, userHome string) error {
	if !isKnown(key) {
		return fmt.Errorf("%w %q (known: %s)", ErrUnknownKey, key, strings.Join(knownNames(), ", "))
	}
	raw := map[string]json.RawMessage{}
	data, err := os.ReadFile(filepath.Clean(path))
	switch {
	case err == nil:
		if raw, err = decodeRaw(data); err != nil {
			return err
		}
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}

	if value == "" {
		delete(raw, key)
	} else {
		var probe Settings
		if applyErr := applySetting(&probe, key, value, userHome); applyErr != nil {
			return applyErr
		}
		enc, encErr := encodeValue(key, value)
		if encErr != nil {
			return encErr
		}
		raw[key] = enc
	}

	out, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fileutil.WriteAtomic(path, append(out, '\n'), 0o600)
}

// applySetting parses value and stores it as setting key in s.
func applySetting(s *Settings, key, value, userHome string) error {
	switch key {
	case KeyLibrary:
		v := strings.TrimSpace(value)
		if v == "" {
			return fmt.Errorf("%w: library path is empty", ErrInvalidValue)
		}
		s.Library = ExpandHome(v, userHome)
	case KeyCapture:
		v := strings.ToLower(strings.TrimSpace(value))
		if !slices.Contains([]string{CaptureAuto, CaptureTap, CaptureDevice}, v) {
			return fmt.Errorf("%w: capture %q (want auto, tap or device)", ErrInvalidValue, value)
		}
		s.Capture = v
	case KeyDevice:
		v := strings.TrimSpace(value)
		if v == "" {
			return fmt.Errorf("%w: device name is empty", ErrInvalidValue)
		}
		s.Device = v
	case KeyArtwork:
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("%w: artwork %q (want true or false)", ErrInvalidValue, value)
		}
		s.Artwork = b
	case KeyIdleTimeout:
		d, err := time.ParseDuration(strings.TrimSpace(value))
		if err != nil || d < 0 {
			return fmt.Errorf("%w: idle_timeout %q (want a duration such as 10m, or 0)", ErrInvalidValue, value)
		}
		s.IdleTimeout = d
	default:
		return fmt.Errorf("%w %q", ErrUnknownKey, key)
	}
	return nil
}

// ExpandHome replaces a leading "~" or "~/" in path with userHome and
// returns it absolute (relative paths resolve against the working
// directory) and clean.
func ExpandHome(path, userHome string) string {
	switch {
	case path == "~":
		path = userHome
	case strings.HasPrefix(path, "~/"):
		path = filepath.Join(userHome, path[2:])
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return filepath.Clean(path)
}

func fileValues(f File) map[string]string {
	m := map[string]string{}
	put := func(k string, v *string) {
		if v != nil {
			m[k] = *v
		}
	}
	put(KeyLibrary, f.Library)
	put(KeyCapture, f.Capture)
	put(KeyDevice, f.Device)
	put(KeyIdleTimeout, f.IdleTimeout)
	if f.Artwork != nil {
		m[KeyArtwork] = strconv.FormatBool(*f.Artwork)
	}
	return m
}

func envValues(getenv func(string) string) map[string]string {
	m := map[string]string{}
	if getenv == nil {
		return m
	}
	for _, k := range Keys() {
		if v := getenv(k.Env); v != "" {
			m[k.Name] = v
		}
	}
	return m
}

func flagValues(o Overrides) map[string]string {
	m := map[string]string{}
	if o.Library != nil {
		m[KeyLibrary] = *o.Library
	}
	if o.Capture != nil {
		m[KeyCapture] = *o.Capture
	}
	if o.Device != nil {
		m[KeyDevice] = *o.Device
	}
	if o.Artwork != nil {
		m[KeyArtwork] = strconv.FormatBool(*o.Artwork)
	}
	if o.IdleTimeout != nil {
		m[KeyIdleTimeout] = o.IdleTimeout.String()
	}
	return m
}

// encodeValue renders a validated value as the JSON type its key uses.
func encodeValue(key, value string) (json.RawMessage, error) {
	if key == KeyArtwork {
		b, err := strconv.ParseBool(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("%w: artwork %q", ErrInvalidValue, value)
		}
		return json.Marshal(b)
	}
	v := strings.TrimSpace(value)
	if key == KeyCapture {
		v = strings.ToLower(v)
	}
	return json.Marshal(v)
}

func decodeRaw(data []byte) (map[string]json.RawMessage, error) {
	raw := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(data)) == 0 {
		return raw, nil
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrDecode, err)
	}
	return raw, nil
}

func isKnown(key string) bool { return slices.Contains(knownNames(), key) }

func knownNames() []string {
	keys := Keys()
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.Name)
	}
	return names
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
