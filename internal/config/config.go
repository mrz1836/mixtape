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

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/fileutil"
)

// Setting names, as written in config.json and accepted by `config set`.
const (
	KeyLibrary     = "library"
	KeyCapture     = "capture"
	KeyDevice      = "device"
	KeyArtwork     = "artwork"
	KeyIdleTimeout = "idle_timeout"
	KeyMaxFailures = "max_failures"
)

// Environment variables.
const (
	EnvHome        = "MIXTAPE_HOME"
	EnvLibrary     = "MIXTAPE_LIBRARY"
	EnvCapture     = "MIXTAPE_CAPTURE"
	EnvDevice      = "MIXTAPE_DEVICE"
	EnvArtwork     = "MIXTAPE_ARTWORK"
	EnvIdleTimeout = "MIXTAPE_IDLE_TIMEOUT"
	EnvMaxFailures = "MIXTAPE_MAX_FAILURES"
)

// Defaults.
const (
	DefaultHomeDir     = ".mixtape"
	DefaultLibraryDir  = "Music/Mixtape"
	DefaultDevice      = "BlackHole"
	DefaultIdleTimeout = 10 * time.Minute
	DefaultMaxFailures = 3
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
	MaxFailures *int    `json:"max_failures,omitempty"`
}

// Overrides carries the values of command-line flags that were set. A nil
// field means the flag was not given.
type Overrides struct {
	Library     *string
	Capture     *string
	Device      *string
	Artwork     *bool
	IdleTimeout *time.Duration
	MaxFailures *int
}

// Settings is the resolved configuration.
type Settings struct {
	Library     string
	Capture     capture.Method
	Device      string
	Artwork     bool
	IdleTimeout time.Duration
	MaxFailures int
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

// setting is everything about one key: its description, how a value from
// any layer is parsed into Settings and shown back, how config.json stores
// it, and which field of File and of Overrides holds it.
type setting struct {
	Key

	// apply parses value (as text, from any layer) into s.
	apply func(s *Settings, value, userHome string) error
	// show renders the resolved value, for `config show`.
	show func(s Settings) string
	// encode is a value apply accepted, as config.json stores it.
	encode func(value string, parsed Settings) any
	// inFile and inFlag return the field holding the setting (a **T).
	inFile func(f *File) any
	inFlag func(o *Overrides) any
}

// table lists every setting in display order. Every function of this
// package that handles settings goes through it.
func table() []setting {
	trimmed := func(value string, _ Settings) any { return strings.TrimSpace(value) }
	return []setting{
		{
			Key: Key{KeyLibrary, EnvLibrary, "~/" + DefaultLibraryDir, "Folder that holds every recording and manifest"},
			apply: func(s *Settings, value, userHome string) error {
				v, err := nonEmpty(value, "library path")
				if err != nil {
					return err
				}
				s.Library = ExpandHome(v, userHome)
				return nil
			},
			show:   func(s Settings) string { return s.Library },
			encode: trimmed,
			inFile: func(f *File) any { return &f.Library },
			inFlag: func(o *Overrides) any { return &o.Library },
		},
		{
			Key: Key{
				KeyCapture, EnvCapture, string(capture.MethodAuto),
				"Capture method: auto, tap (Core Audio process tap) or device (BlackHole)",
			},
			apply: func(s *Settings, value, _ string) error {
				m, err := capture.ParseMethod(value)
				if err != nil || strings.TrimSpace(value) == "" {
					return fmt.Errorf("%w: capture %q (want auto, tap or device)", ErrInvalidValue, value)
				}
				s.Capture = m
				return nil
			},
			show:   func(s Settings) string { return string(s.Capture) },
			encode: func(_ string, s Settings) any { return string(s.Capture) },
			inFile: func(f *File) any { return &f.Capture },
			inFlag: func(o *Overrides) any { return &o.Capture },
		},
		{
			Key: Key{KeyDevice, EnvDevice, DefaultDevice, "Capture device name (substring) for the device method"},
			apply: func(s *Settings, value, _ string) error {
				v, err := nonEmpty(value, "device name")
				if err != nil {
					return err
				}
				s.Device = v
				return nil
			},
			show:   func(s Settings) string { return s.Device },
			encode: trimmed,
			inFile: func(f *File) any { return &f.Device },
			inFlag: func(o *Overrides) any { return &o.Device },
		},
		{
			Key: Key{KeyArtwork, EnvArtwork, "false", "Embed album artwork in each file (one image download per song)"},
			apply: func(s *Settings, value, _ string) error {
				b, err := strconv.ParseBool(strings.TrimSpace(value))
				if err != nil {
					return fmt.Errorf("%w: artwork %q (want true or false)", ErrInvalidValue, value)
				}
				s.Artwork = b
				return nil
			},
			show:   func(s Settings) string { return strconv.FormatBool(s.Artwork) },
			encode: func(_ string, s Settings) any { return s.Artwork },
			inFile: func(f *File) any { return &f.Artwork },
			inFlag: func(o *Overrides) any { return &o.Artwork },
		},
		{
			Key: Key{
				KeyIdleTimeout, EnvIdleTimeout, DefaultIdleTimeout.String(),
				"End a session after this long with nothing recording (0 = never)",
			},
			apply: func(s *Settings, value, _ string) error {
				d, err := time.ParseDuration(strings.TrimSpace(value))
				if err != nil || d < 0 {
					return fmt.Errorf("%w: idle_timeout %q (want a duration such as 10m, or 0)", ErrInvalidValue, value)
				}
				s.IdleTimeout = d
				return nil
			},
			show:   func(s Settings) string { return s.IdleTimeout.String() },
			encode: trimmed, // as written: "10m" stays "10m"
			inFile: func(f *File) any { return &f.IdleTimeout },
			inFlag: func(o *Overrides) any { return &o.IdleTimeout },
		},
		{
			Key: Key{
				KeyMaxFailures, EnvMaxFailures, strconv.Itoa(DefaultMaxFailures),
				"End a session after this many songs in a row could not be recorded (0 = never)",
			},
			apply: func(s *Settings, value, _ string) error {
				n, err := strconv.Atoi(strings.TrimSpace(value))
				if err != nil || n < 0 {
					return fmt.Errorf("%w: max_failures %q (want a whole number, or 0 for never)", ErrInvalidValue, value)
				}
				s.MaxFailures = n
				return nil
			},
			show:   func(s Settings) string { return strconv.Itoa(s.MaxFailures) },
			encode: func(_ string, s Settings) any { return s.MaxFailures },
			inFile: func(f *File) any { return &f.MaxFailures },
			inFlag: func(o *Overrides) any { return &o.MaxFailures },
		},
	}
}

// lookup returns the setting named key.
func lookup(key string) (setting, bool) {
	for _, st := range table() {
		if st.Name == key {
			return st, true
		}
	}
	return setting{}, false
}

// Keys lists every setting in display order.
func Keys() []Key {
	t := table()
	keys := make([]Key, 0, len(t))
	for _, st := range t {
		keys = append(keys, st.Key)
	}
	return keys
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
	// Only exact key names count: JSON decoding alone would also take
	// "LIBRARY" for library, against the warning that it is ignored.
	var (
		f        File
		warnings []string
	)
	for _, k := range sortedKeys(raw) {
		st, ok := lookup(k)
		if !ok {
			warnings = append(warnings, fmt.Sprintf("%s: ignoring unknown key %q", path, k))
			continue
		}
		if err := json.Unmarshal(raw[k], st.inFile(&f)); err != nil {
			return File{}, warnings, fmt.Errorf("%w: %s: %s: %w", ErrDecode, path, k, err)
		}
	}
	return f, warnings, nil
}

// Resolve layers the environment and flag overrides over the file and the
// defaults, validates the result, and expands a leading ~/ in the library
// path against userHome.
func Resolve(f File, getenv func(string) string, o Overrides, userHome string) (Settings, error) {
	s := Settings{
		Library:     filepath.Join(userHome, DefaultLibraryDir),
		Capture:     capture.MethodAuto,
		Device:      DefaultDevice,
		IdleTimeout: DefaultIdleTimeout,
		MaxFailures: DefaultMaxFailures,
		Sources:     map[string]Source{},
	}
	layers := []struct {
		src   Source
		value func(st setting) (string, bool)
	}{
		{SourceFile, func(st setting) (string, bool) { return fieldText(st.inFile(&f)) }},
		{SourceEnv, func(st setting) (string, bool) {
			if getenv == nil {
				return "", false
			}
			v := getenv(st.Env)
			return v, v != ""
		}},
		{SourceFlag, func(st setting) (string, bool) { return fieldText(st.inFlag(&o)) }},
	}
	for _, st := range table() {
		s.Sources[st.Name] = SourceDefault
		for _, layer := range layers {
			v, ok := layer.value(st)
			if !ok {
				continue
			}
			if err := st.apply(&s, v, userHome); err != nil {
				return Settings{}, fmt.Errorf("%w (from %s)", err, layer.src)
			}
			s.Sources[st.Name] = layer.src
		}
	}
	return s, nil
}

// fieldText renders the value of a File or Overrides field (a **T) as
// text; ok is false when the field is unset.
func fieldText(field any) (string, bool) {
	switch p := field.(type) {
	case **string:
		if *p != nil {
			return **p, true
		}
	case **bool:
		if *p != nil {
			return strconv.FormatBool(**p), true
		}
	case **int:
		if *p != nil {
			return strconv.Itoa(**p), true
		}
	case **time.Duration:
		if *p != nil {
			return (**p).String(), true
		}
	}
	return "", false
}

// Value returns a setting rendered as text, for `config show`.
func (s Settings) Value(key string) string {
	if st, ok := lookup(key); ok {
		return st.show(s)
	}
	return ""
}

// Set validates value for key and writes it to the config file at path,
// creating the file (and its directory) if needed. An empty value removes
// the key, restoring the default. Keys Mixtape does not know are kept as
// they are, so a downgrade never loses settings.
func Set(path, key, value, userHome string) error {
	st, ok := lookup(key)
	if !ok {
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
		return fmt.Errorf("read config: %w", err)
	}

	if value == "" {
		delete(raw, key)
	} else {
		var parsed Settings
		if err := st.apply(&parsed, value, userHome); err != nil {
			return err
		}
		enc, err := json.Marshal(st.encode(value, parsed))
		if err != nil {
			return fmt.Errorf("encode %s: %w", key, err)
		}
		raw[key] = enc
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := fileutil.WriteJSON(path, raw, 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	return nil
}

// nonEmpty trims value and rejects an empty one, naming what it is.
func nonEmpty(value, what string) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%w: %s is empty", ErrInvalidValue, what)
	}
	return v, nil
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
