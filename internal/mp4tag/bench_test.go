package mp4tag

import (
	"bytes"
	"testing"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// BenchmarkTag measures retagging a file the size of a 4-minute song
// (about 7.5 MB of 256 kb/s AAC) with a full tag set and a cover: the work
// every recording and every copy does once.
func BenchmarkTag(b *testing.B) {
	data := testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0x5a}, 7_500_000), Chunks: 2000})
	tags := Tags{
		Title: "Title", Artist: "Artist", AlbumArtist: "Artist", Album: "Album", Grouping: "Playlist",
		Comment: "https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT", TrackNumber: 3, TrackTotal: 12,
		DiscNumber: 1, Date: "1987-07-27", Cover: append(testutil.JPEG(), make([]byte, 64<<10)...),
		Freeform: map[string]string{"SPOTIFY_ID": "4cOdK2wGLETKBW3PvgPWqT", "MIXTAPE_AUDIO_SHA256": "x"},
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Tag(data, tags); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAudioSHA256 measures hashing the audio of the same file.
func BenchmarkAudioSHA256(b *testing.B) {
	data := testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0x5a}, 7_500_000)})
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := AudioSHA256(data); err != nil {
			b.Fatal(err)
		}
	}
}
