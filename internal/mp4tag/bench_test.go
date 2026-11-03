package mp4tag

import (
	"bytes"
	"io"
	"testing"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// benchTags is a full tag set with a cover.
func benchTags() Tags {
	return Tags{
		Title: "Title", Artist: "Artist", AlbumArtist: "Artist", Album: "Album", Grouping: "Playlist",
		Comment: "https://open.spotify.com/track/4cOdK2wGLETKBW3PvgPWqT", TrackNumber: 3, TrackTotal: 12,
		DiscNumber: 1, Date: "1987-07-27", Cover: append(testutil.JPEG(), make([]byte, 64<<10)...),
		Freeform: map[string]string{"SPOTIFY_ID": "4cOdK2wGLETKBW3PvgPWqT", "MIXTAPE_AUDIO_SHA256": "x"},
	}
}

// BenchmarkTag measures retagging a file the size of a 4-minute song
// (about 7.5 MB of 256 kb/s AAC) in memory with a full tag set and a cover.
func BenchmarkTag(b *testing.B) {
	data := testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0x5a}, 7_500_000), Chunks: 2000})
	tags := benchTags()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, err := Tag(data, tags); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkTagFile measures what every recording and every copy does once:
// retagging a 4-minute 24-bit ALAC song (about 40 MB) from disk, streamed
// to a writer that discards it, so only the moov is held in memory.
func BenchmarkTagFile(b *testing.B) {
	data := testutil.M4A(testutil.M4AOptions{Audio: bytes.Repeat([]byte{0x5a}, 40_000_000), Chunks: 2000, Codec: "alac"})
	path := testutil.WriteFile(b, "song.m4a", data)
	tags := benchTags()
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		r, err := TagFile(path, tags)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := r.WriteTo(io.Discard); err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
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
