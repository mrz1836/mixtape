package spotify

import (
	"bytes"
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Spotify desktop preference keys.
const (
	prefCrossfade     = "audio.crossfade_v2"
	prefCrossfadeTime = "audio.crossfade.time_v2" // milliseconds
	prefMigrated      = "audio.crossfade_migrated_to_ps"
	prefAutomixWord   = "automix"
)

// spotifySupportDir is Spotify's data folder under the user's home.
const spotifySupportDir = "Library/Application Support/Spotify"

// ReadPrefs reads the playback preferences of the Spotify account used
// most recently on this Mac: the newest Users/*/prefs file, falling back to
// the global prefs file. No prefs file at all yields zero Prefs and no
// error. Reading is best-effort: Spotify owns the format.
func ReadPrefs(userHome string) (Prefs, error) {
	base := filepath.Join(userHome, spotifySupportDir)
	path, err := newestPrefs(base)
	if err != nil {
		return Prefs{}, err
	}
	if path == "" {
		return Prefs{}, nil
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Prefs{}, err
	}
	p := parsePrefs(data)
	p.Path = path
	return p, nil
}

// newestPrefs returns the prefs file to read, or "" when there is none.
func newestPrefs(base string) (string, error) {
	matches, err := filepath.Glob(filepath.Join(base, "Users", "*", "prefs"))
	if err != nil {
		return "", err
	}
	var (
		best     string
		bestTime time.Time
	)
	for _, m := range matches {
		info, statErr := os.Stat(m)
		if statErr != nil || !info.Mode().IsRegular() {
			continue
		}
		if best == "" || info.ModTime().After(bestTime) {
			best, bestTime = m, info.ModTime()
		}
	}
	if best != "" {
		return best, nil
	}
	global := filepath.Join(base, "prefs")
	if _, statErr := os.Stat(global); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return "", nil
		}
		return "", statErr
	}
	return global, nil
}

// parsePrefs reads key=value lines, however long.
func parsePrefs(data []byte) Prefs {
	var p Prefs
	for line := range bytes.Lines(data) {
		key, value, ok := strings.Cut(strings.TrimSpace(string(line)), "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"`)
		switch {
		case key == prefCrossfade:
			p.Crossfade = value == "true"
		case key == prefMigrated:
			p.Stale = value == "true"
		case key == prefCrossfadeTime:
			// ParseFloat accepts "Inf", which is no crossfade length.
			if ms, err := strconv.ParseFloat(value, 64); err == nil && ms > 0 && !math.IsInf(ms, 0) {
				p.CrossfadeSeconds = ms / 1000
			}
		case strings.Contains(strings.ToLower(key), prefAutomixWord) && value == "true":
			p.Automix = true
		}
	}
	return p
}
