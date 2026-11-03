package library

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
)

func TestSanitize(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Still Loving You", "Still Loving You"},
		{"AC/DC: Back In Black", "ACDC Back In Black"},
		{`What?"*<>|`, "What"},
		{"  multiple   spaces\tand\nnewlines ", "multiple spaces and newlines"},
		{"ctrl\x00\x1fchars\x7f", "ctrlchars"},
		{"", ""},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, Sanitize(c.in), "Sanitize(%q)", c.in)
	}
}

func TestFilenameSafe(t *testing.T) {
	t.Parallel()
	cases := []struct{ in, want string }{
		{"Céleste - Lofi Remix", "Céleste-Lofi-Remix"},
		{"Today’s Top Hits", "Today’s-Top-Hits"},
		{".hidden track", "hidden-track"},
		{" - leading dash", "leading-dash"},
		{"", unknownName},
		{"///", unknownName},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, filenameSafe(c.in, maxPartBytes), "filenameSafe(%q)", c.in)
	}
}

func TestCapBytesKeepsRunesWhole(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "abc", capBytes("abc", 10))
	got := capBytes(strings.Repeat("€", 50), 100) // 3 bytes per rune
	assert.LessOrEqual(t, len(got), 100)
	assert.True(t, utf8.ValidString(got))
	assert.Len(t, got, 99)
}

func TestTrackFileName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Pink-Floyd-Time.m4a", trackFileName(KindPlaylist, 4, "Pink Floyd", "Time"))
	assert.Equal(t, "Pink-Floyd-Time.m4a", trackFileName(KindSingles, 0, "Pink Floyd", "Time"))
	assert.Equal(t, "04-Pink-Floyd-Time.m4a", trackFileName(KindAlbum, 4, "Pink Floyd", "Time"))
	assert.Equal(t, "123-A-B.m4a", trackFileName(KindAlbum, 123, "A", "B"))
	assert.Equal(t, "Unknown-Unknown.m4a", trackFileName(KindPlaylist, 1, "", ""))

	long := trackFileName(KindAlbum, 7, strings.Repeat("Ryūichi€", 40), strings.Repeat("🎹", 80))
	assert.LessOrEqual(t, len(withSuffix(long, "abcdef")), 255, "fits APFS with a collision suffix")
	assert.True(t, utf8.ValidString(long))
	assert.True(t, strings.HasSuffix(long, ".m4a"))
}

func TestWithSuffix(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "A-B-4cOdK2.m4a", withSuffix("A-B.m4a", shortID("4cOdK2wGLETKBW3PvgPWqT")))
	assert.Equal(t, "ab", shortID("ab"))
}

func TestFolderName(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "Today’s-Top-Hits", folderName(Spec{Kind: KindPlaylist, Name: "Today’s Top Hits"}))
	assert.Equal(t, "Pink-Floyd-The-Dark-Side-of-the-Moon",
		folderName(Spec{Kind: KindAlbum, Name: "The Dark Side of the Moon", Artist: "Pink Floyd"}))
	assert.Equal(t, "Abbey-Road", folderName(Spec{Kind: KindAlbum, Name: "Abbey Road"}))
	assert.Equal(t, "Singles", folderName(Spec{Kind: KindSingles, Name: "whatever"}))
	assert.Equal(t, "Road-Trip", folderName(Spec{Kind: KindFolder, Name: "Road Trip"}))
}
