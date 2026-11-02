package library

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/wav"
)

// Public track IDs used across the tests.
const (
	idTime  = "3TO7bbrUKrOSPGRTB5MeCz" // Pink Floyd - Time
	idMoney = "0vFOzaXqZHahrZp6enQwQb" // Pink Floyd - Money
	idRick  = "4cOdK2wGLETKBW3PvgPWqT" // Rick Astley - Never Gonna Give You Up
	idAlbum = "4LH4d3cOWNNsVw41Gqt2kv" // The Dark Side of the Moon
	idList  = "37i9dQZF1DXcBWIGoYBM5M" // Today's Top Hits
)

var errFakeEncode = errors.New("fake encoder failure")

// requireAAC skips the test unless afconvert can encode AAC here (the
// encoder is invisible inside a sandbox). afconvert -hf exits non-zero
// even on success, so only its listing counts.
func requireAAC(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	out, _ := exec.CommandContext(t.Context(), "afconvert", "-hf").CombinedOutput()
	if !strings.Contains(string(out), "'aac '") {
		t.Skip("afconvert cannot encode AAC here (sandboxed?)")
	}
}

// fixture is a library in a temp dir with captured warnings.
type fixture struct {
	t     *testing.T
	lib   *Library
	root  string
	work  string
	now   time.Time
	mu    sync.Mutex
	warns []string
}

func newFixture(t *testing.T, enc Encoder) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir(), work: t.TempDir(), now: time.Date(2026, 11, 2, 9, 30, 0, 0, time.UTC)}
	f.lib = f.open(enc)
	return f
}

func (f *fixture) open(enc Encoder) *Library {
	f.t.Helper()
	lib, err := Open(f.root, Options{
		Encoder: enc,
		WorkDir: f.work,
		Now:     func() time.Time { return f.now },
		Warn: func(msg string) {
			f.mu.Lock()
			f.warns = append(f.warns, msg)
			f.mu.Unlock()
		},
	})
	require.NoError(f.t, err)
	return lib
}

// wav writes a short stereo sine WAV at freq Hz; distinct frequencies give
// distinct audio hashes.
func (f *fixture) wav(freq float64) string {
	f.t.Helper()
	const rate = 44100
	n := rate / 5 // 0.2s
	data := make([]byte, 0, n*4)
	for i := range n {
		v := int16(12000 * math.Sin(2*math.Pi*freq*float64(i)/rate))
		data = append(data, byte(v), byte(v>>8), byte(v), byte(v>>8))
	}
	path := filepath.Join(f.t.TempDir(), "rec.wav")
	file, err := os.Create(path)
	require.NoError(f.t, err)
	require.NoError(f.t, wav.Write(file, data, rate, 2))
	require.NoError(f.t, file.Close())
	return path
}

// commit records t into c from a fresh WAV at freq.
func (f *fixture) commit(c *Collection, t Track, freq float64, o CommitOptions) Entry {
	f.t.Helper()
	e, err := f.lib.Commit(f.t.Context(), c, t, f.wav(freq), o)
	require.NoError(f.t, err)
	return e
}

func (f *fixture) collection(s Spec) *Collection {
	f.t.Helper()
	c, err := f.lib.Collection(s)
	require.NoError(f.t, err)
	return c
}

func (f *fixture) tags(path string) mp4tag.Tags {
	f.t.Helper()
	tags, err := mp4tag.ReadFile(path)
	require.NoError(f.t, err)
	return tags
}

func playlistSpec() Spec {
	return Spec{
		Kind: KindPlaylist, SpotifyID: idList, Name: "Today’s Top Hits", Owner: "Spotify",
		URL: "https://open.spotify.com/playlist/" + idList,
	}
}

func albumSpec() Spec {
	return Spec{Kind: KindAlbum, SpotifyID: idAlbum, Name: "The Dark Side of the Moon", Artist: "Pink Floyd"}
}

func trackTime() Track {
	return Track{
		ID: idTime, Title: "Time", Artist: "Pink Floyd", Album: "The Dark Side of the Moon",
		AlbumArtist: "Pink Floyd", DurationMS: 413000, TrackNumber: 4, DiscNumber: 1,
	}
}

func trackMoney() Track {
	return Track{ID: idMoney, Title: "Money", Artist: "Pink Floyd", DurationMS: 382000, TrackNumber: 6}
}

// failEncoder always fails.
type failEncoder struct{}

func (failEncoder) Encode(context.Context, string, string) error { return errFakeEncode }
