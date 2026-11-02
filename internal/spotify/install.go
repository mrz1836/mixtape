package spotify

import (
	"os"
	"path/filepath"
)

// InstalledPath returns the Spotify.app bundle path when the app is
// installed in /Applications or the user's ~/Applications.
func InstalledPath(userHome string) (string, bool) {
	candidates := []string{"/Applications/Spotify.app"}
	if userHome != "" {
		candidates = append(candidates, filepath.Join(userHome, "Applications", "Spotify.app"))
	}
	return firstDir(candidates)
}

// firstDir returns the first candidate that is an existing directory.
func firstDir(candidates []string) (string, bool) {
	for _, p := range candidates {
		if info, err := os.Stat(p); err == nil && info.IsDir() {
			return p, true
		}
	}
	return "", false
}
