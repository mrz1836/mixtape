package library

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/mrz1836/mixtape/internal/fileutil"
)

// Freeform tag names Mixtape writes into every file, so a file identifies
// itself without its manifest.
const (
	// TagSpotifyID is the ID of the track actually recorded.
	TagSpotifyID = "SPOTIFY_ID"
	// TagRequestedID is the collection's track ID, written only when
	// Spotify played a relinked track under another ID.
	TagRequestedID = "SPOTIFY_REQUESTED_ID"
	// TagAudioSHA256 is the SHA-256 of the AAC audio payload.
	TagAudioSHA256 = "MIXTAPE_AUDIO_SHA256"
	// TagPartial is "1" on a song kept with --keep-partial.
	TagPartial = "MIXTAPE_PARTIAL"
)

const (
	lockName       = ".mixtape.lock"
	defaultBitrate = 256000
	httpTimeout    = 10 * time.Second
	manifestPerm   = 0o644
	audioPerm      = 0o644
	dirPerm        = 0o750
)

var (
	errSpec         = errors.New("invalid collection spec")
	errNoEntry      = errors.New("no such track in the collection")
	errNoPendingWAV = errors.New("no kept WAV to re-encode")
)

// Options configures a Library.
type Options struct {
	// Encoder turns a WAV into an M4A; nil means AFConvert{}.
	Encoder Encoder
	// WorkDir holds encoder scratch files; WAVs that failed to encode are
	// kept in WorkDir/failed. Empty means a folder in os.TempDir().
	WorkDir string
	// HTTPClient downloads artwork; nil means a client with a 10s timeout.
	HTTPClient *http.Client
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Warn receives non-fatal problems; nil discards them.
	Warn func(msg string)
}

// Library is the folder of recordings. It is not safe for concurrent use:
// one goroutine drives it.
//
// Open only reads. Writers (recording, copying, verify --fix) must hold
// Lock, which also applies the repairs Open found: it removes temp files a
// crashed write left behind and saves manifests that were reconciled with
// the files on disk.
type Library struct {
	root string
	opts Options
	cols []*Collection
}

// Open loads every collection folder under root (one level deep),
// reconciling each manifest with the files that exist. root is created if
// missing. An unreadable manifest is skipped with a warning.
func Open(root string, o Options) (*Library, error) {
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, dirPerm); err != nil {
		return nil, fmt.Errorf("create library: %w", err)
	}
	if o.Encoder == nil {
		o.Encoder = AFConvert{}
	}
	if o.WorkDir == "" {
		o.WorkDir = filepath.Join(os.TempDir(), "mixtape-work")
	}
	if o.HTTPClient == nil {
		o.HTTPClient = &http.Client{Timeout: httpTimeout}
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Warn == nil {
		o.Warn = func(string) {}
	}
	l := &Library{root: root, opts: o}

	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("read library: %w", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, d.Name())
		m, loadErr := loadManifest(filepath.Join(dir, ManifestName))
		if errors.Is(loadErr, fs.ErrNotExist) {
			continue
		}
		if loadErr != nil {
			l.warn("skipping %s: %v", d.Name(), loadErr)
			continue
		}
		c := &Collection{Dir: dir, Manifest: m}
		c.dirty = l.reconcile(c)
		l.cols = append(l.cols, c)
	}
	l.sort()
	return l, nil
}

// Root returns the library folder.
func (l *Library) Root() string { return l.root }

// Lock takes the library's writer lock (an flock on <root>/.mixtape.lock)
// without waiting, then applies the repairs Open found. It returns
// ErrLocked when another process holds it. Call unlock when done.
func (l *Library) Lock() (unlock func(), err error) {
	unlock, err = lockFile(filepath.Join(l.root, lockName))
	if err != nil {
		return nil, err
	}
	for _, c := range l.cols {
		l.cleanTemps(c)
		if !c.dirty {
			continue
		}
		if saveErr := l.Save(c); saveErr != nil {
			unlock()
			return nil, saveErr
		}
		for _, w := range c.staleWAVs {
			_ = os.Remove(w)
		}
		c.staleWAVs = nil
	}
	return unlock, nil
}

// Collections returns every collection, sorted by folder name.
func (l *Library) Collections() []*Collection { return slices.Clone(l.cols) }

// Spec identifies a collection to find or create.
type Spec struct {
	Kind      Kind
	SpotifyID string // required for playlists and albums
	Name      string // required for folders
	Owner     string
	URL       string
	Artist    string // album artist, used in album folder names
}

// Collection returns the collection matching s, creating its folder and an
// empty manifest when there is none. A playlist or album is found by its
// Spotify ID, so a renamed playlist keeps its folder; its name, owner and
// URL are refreshed from s.
func (l *Library) Collection(s Spec) (*Collection, error) {
	switch s.Kind {
	case KindPlaylist, KindAlbum:
		if s.SpotifyID == "" {
			return nil, fmt.Errorf("%w: %s without a Spotify ID", errSpec, s.Kind)
		}
	case KindFolder:
		if Sanitize(s.Name) == "" {
			return nil, fmt.Errorf("%w: folder without a name", errSpec)
		}
	case KindSingles:
		if s.Name == "" {
			s.Name = singlesFolder
		}
	default:
		return nil, fmt.Errorf("%w: kind %q", errSpec, s.Kind)
	}

	if c := l.lookup(s); c != nil {
		if c.refresh(s) {
			if err := l.Save(c); err != nil {
				return nil, err
			}
		}
		return c, nil
	}

	dir, err := l.newDir(s)
	if err != nil {
		return nil, err
	}
	c := &Collection{Dir: dir, Manifest: Manifest{
		Schema:    Schema,
		Kind:      s.Kind,
		SpotifyID: s.SpotifyID,
		Name:      s.Name,
		Owner:     s.Owner,
		URL:       s.URL,
		Tracks:    []Entry{},
	}}
	if err := l.Save(c); err != nil {
		return nil, err
	}
	l.cols = append(l.cols, c)
	l.sort()
	return c, nil
}

// Lookup returns the existing collection matching s, without creating or
// changing anything (for dry runs and read-only commands).
func (l *Library) Lookup(s Spec) (*Collection, bool) {
	if s.Kind == KindSingles && s.Name == "" {
		s.Name = singlesFolder
	}
	c := l.lookup(s)
	return c, c != nil
}

func (l *Library) lookup(s Spec) *Collection {
	for _, c := range l.cols {
		m := c.Manifest
		if m.Kind != s.Kind {
			continue
		}
		switch s.Kind {
		case KindPlaylist, KindAlbum:
			if m.SpotifyID == s.SpotifyID {
				return c
			}
		case KindSingles:
			return c
		case KindFolder:
			if strings.EqualFold(m.Name, s.Name) {
				return c
			}
		}
	}
	return nil
}

// newDir creates and returns the folder for a new collection. A name that
// is taken gets the Spotify ID's first characters, or a number. Singles and
// named folders adopt an existing folder that has no manifest.
func (l *Library) newDir(s Spec) (string, error) {
	base := folderName(s)
	candidates := []string{base}
	if s.SpotifyID != "" {
		candidates = append(candidates, base+"-"+shortID(s.SpotifyID))
	}
	for i := 2; i < 1000; i++ {
		candidates = append(candidates, base+"-"+strconv.Itoa(i))
	}
	for _, name := range candidates {
		dir := filepath.Join(l.root, name)
		info, err := os.Stat(dir)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			if mkErr := os.MkdirAll(dir, dirPerm); mkErr != nil {
				return "", fmt.Errorf("create collection folder: %w", mkErr)
			}
			return dir, nil
		case err != nil:
			return "", err
		case info.IsDir() && (s.Kind == KindSingles || s.Kind == KindFolder) && !exists(filepath.Join(dir, ManifestName)):
			return dir, nil
		}
	}
	return "", fmt.Errorf("%w: no free folder name for %q", errSpec, base)
}

// Location is a done song somewhere in the library.
type Location struct {
	Collection *Collection
	Entry      Entry
	Path       string
}

// Find returns every complete song whose requested or played ID is id and
// whose file exists, across all collections.
func (l *Library) Find(id string) []Location {
	if id == "" {
		return nil
	}
	return l.locate(func(e *Entry) bool { return e.RequestedID == id || e.PlayedID == id })
}

// FindHash returns every complete song whose audio hash is sha.
func (l *Library) FindHash(sha string) []Location {
	if sha == "" {
		return nil
	}
	return l.locate(func(e *Entry) bool { return e.AudioSHA256 == sha })
}

func (l *Library) locate(match func(*Entry) bool) []Location {
	var out []Location
	for _, c := range l.cols {
		for i := range c.Manifest.Tracks {
			e := &c.Manifest.Tracks[i]
			if !e.complete() || !match(e) {
				continue
			}
			if path := c.Path(e); exists(path) {
				out = append(out, Location{Collection: c, Entry: *e, Path: path})
			}
		}
	}
	return out
}

// Mark sets a track's status and failure reason and saves the manifest.
func (l *Library) Mark(c *Collection, id string, status Status, reason string) error {
	e := c.Entry(id)
	if e == nil {
		return fmt.Errorf("%w: %s in %s", errNoEntry, id, c.Folder())
	}
	e.Status, e.FailReason = status, reason
	return l.Save(c)
}

// Save writes c's manifest atomically and regenerates its .m3u8.
func (l *Library) Save(c *Collection) error {
	c.sortTracks()
	m := &c.Manifest
	m.Schema = Schema
	m.UpdatedAt = l.opts.Now().UTC().Round(time.Second)
	if m.Tracks == nil {
		m.Tracks = []Entry{}
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := fileutil.WriteAtomic(filepath.Join(c.Dir, ManifestName), append(data, '\n'), manifestPerm); err != nil {
		return fmt.Errorf("save manifest: %w", err)
	}
	if err := fileutil.WriteAtomic(filepath.Join(c.Dir, c.Folder()+m3u8Ext), c.playlist(), manifestPerm); err != nil {
		return fmt.Errorf("save playlist: %w", err)
	}
	c.dirty = false
	return nil
}

func loadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, fmt.Errorf("%w: %w", ErrBadManifest, err)
	}
	if m.Schema > Schema {
		return Manifest{}, fmt.Errorf("%w: schema %d is newer than this mixtape (%d)", ErrBadManifest, m.Schema, Schema)
	}
	return m, nil
}

func (l *Library) sort() {
	slices.SortFunc(l.cols, func(a, b *Collection) int {
		return cmp.Or(
			cmp.Compare(strings.ToLower(a.Folder()), strings.ToLower(b.Folder())),
			cmp.Compare(a.Folder(), b.Folder()),
		)
	})
}

func (l *Library) warn(format string, args ...any) { l.opts.Warn(fmt.Sprintf(format, args...)) }

// rel returns path relative to the library root, or path itself.
func (l *Library) rel(path string) string {
	if r, err := filepath.Rel(l.root, path); err == nil {
		return r
	}
	return path
}

func exists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
