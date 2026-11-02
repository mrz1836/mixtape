package library

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Name limits. APFS allows 255 bytes per name; the caps leave room for the
// collision suffix and the extension.
const (
	maxPartBytes   = 100 // one artist or title part of a file name
	maxNameBytes   = 240 // a whole file name, extension included
	maxFolderBytes = 200 // a collection folder name
	idSuffixLen    = 6   // characters of a Spotify ID used to break name collisions
	m4aExt         = ".m4a"
	m3u8Ext        = ".m3u8"
	singlesFolder  = "Singles"
	unknownName    = "Unknown"
)

// Sanitize makes s safe as (part of) a file name: it drops the characters
// macOS and its tools reject (`/ : " ? * < > |`) and control characters
// (whitespace controls become spaces), then collapses runs of whitespace.
func Sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 32 || r == 127 {
			// Whitespace controls (tab, newline…) become a space so words do
			// not get glued together; other controls are dropped.
			if unicode.IsSpace(r) {
				b.WriteRune(' ')
			}
			continue
		}
		if strings.ContainsRune(`/:"?*<>|`, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// filenameSafe is Sanitize with spaces turned into dashes and dash runs
// collapsed ("Céleste - Remix" → "Céleste-Remix"), no leading dot (which
// would hide the file), capped at limit bytes, and "Unknown" when nothing
// is left.
func filenameSafe(s string, limit int) string {
	s = strings.ReplaceAll(Sanitize(s), " ", "-")
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.TrimLeft(s, ".")
	s = strings.Trim(capBytes(s, limit), "-")
	if s == "" {
		return unknownName
	}
	return s
}

// capBytes truncates s to at most n bytes without splitting a UTF-8
// sequence.
func capBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for len(s) > 0 && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// MMSS formats a duration as m:ss (e.g. "3:12", "15:04").
func MMSS(d time.Duration) string {
	total := int(d.Seconds())
	return fmt.Sprintf("%d:%02d", total/60, total%60)
}

// trackFileName is the file name for a song: "Artist-Title.m4a", or
// "NN-Artist-Title.m4a" in an album folder, where NN is the album position.
func trackFileName(kind Kind, position int, artist, title string) string {
	base := filenameSafe(artist, maxPartBytes) + "-" + filenameSafe(title, maxPartBytes)
	if kind == KindAlbum && position > 0 {
		width := 2
		if position > 99 {
			width = 3
		}
		base = fmt.Sprintf("%0*d-%s", width, position, base)
	}
	return capBytes(base, maxNameBytes-len(m4aExt)-1-idSuffixLen) + m4aExt
}

// withSuffix inserts "-<suffix>" before the extension of name.
func withSuffix(name, suffix string) string {
	ext := filepath.Ext(name)
	return strings.TrimSuffix(name, ext) + "-" + suffix + ext
}

// shortID returns the first idSuffixLen characters of a Spotify ID.
func shortID(id string) string {
	if len(id) > idSuffixLen {
		return id[:idSuffixLen]
	}
	return id
}

// folderName is the folder a new collection gets.
func folderName(s Spec) string {
	switch s.Kind {
	case KindSingles:
		return singlesFolder
	case KindAlbum:
		if s.Artist != "" {
			return filenameSafe(s.Artist+" - "+s.Name, maxFolderBytes)
		}
	case KindPlaylist, KindFolder:
	}
	return filenameSafe(s.Name, maxFolderBytes)
}
