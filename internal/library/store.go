package library

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mrz1836/mixtape/internal/fileutil"
	"github.com/mrz1836/mixtape/internal/mp4tag"
)

// Artwork downloads are capped; real Spotify covers are well under 1 MiB.
const maxArtworkBytes = 5 << 20

// failReasonMax bounds the error text stored in a manifest.
const failReasonMax = 300

var (
	errEmptyOutput   = errors.New("afconvert produced no output")
	errArtworkURL    = errors.New("artwork URL is not https")
	errArtworkStatus = errors.New("artwork download failed")
	errArtworkSize   = errors.New("artwork is larger than 5 MiB")
)

// Encoder turns a WAV file into an AAC M4A file.
type Encoder interface {
	Encode(ctx context.Context, wavPath, m4aPath string) error
}

// AFConvert encodes with macOS's built-in afconvert.
type AFConvert struct {
	// Bitrate in bits per second; 0 means 256000.
	Bitrate int
}

// Encode runs afconvert -f m4af -d aac -b <bitrate>. If afconvert rejects
// the bitrate (some low sample rates cannot carry 256 kb/s AAC), it retries
// once at afconvert's default bitrate.
func (a AFConvert) Encode(ctx context.Context, wavPath, m4aPath string) error {
	bitrate := a.Bitrate
	if bitrate <= 0 {
		bitrate = defaultBitrate
	}
	err := afconvert(ctx, "-f", "m4af", "-d", "aac", "-b", strconv.Itoa(bitrate), wavPath, m4aPath)
	if err != nil {
		_ = os.Remove(m4aPath)
		if retryErr := afconvert(ctx, "-f", "m4af", "-d", "aac", wavPath, m4aPath); retryErr != nil {
			return err
		}
	}
	info, statErr := os.Stat(m4aPath)
	if statErr != nil || info.Size() == 0 {
		return errEmptyOutput
	}
	return nil
}

func afconvert(ctx context.Context, args ...string) error {
	out, err := exec.CommandContext(ctx, "afconvert", args...).CombinedOutput() //nolint:gosec // G204: fixed binary and flags; the paths are Mixtape's own work files, run without a shell
	if err != nil {
		return fmt.Errorf("afconvert: %w: %s", err, firstLine(string(out)))
	}
	return nil
}

// releaseDateTimeout bounds the release-date lookup of one song.
const releaseDateTimeout = 10 * time.Second

// CommitOptions tune Commit.
type CommitOptions struct {
	Artwork    bool  // download and embed the cover art
	KeepWAV    bool  // keep the source WAV after a successful encode
	Partial    bool  // the song was not captured start to finish (--keep-partial)
	RecordedMS int64 // length of the captured audio
}

// Commit encodes the recording at wavPath, tags it, and stores it as t's
// file in c, replacing any earlier file for t in place. The manifest and
// playlist are saved before it returns.
//
// If encoding fails the WAV moves to WorkDir/failed, the entry becomes
// encode_failed with PendingWAV set (so RetryEncode can finish the job
// without recording again), and the error wraps ErrEncode.
func (l *Library) Commit(ctx context.Context, c *Collection, t Track, wavPath string, o CommitOptions) (Entry, error) {
	if t.ID == "" {
		return Entry{}, fmt.Errorf("%w: track without an ID", errSpec)
	}
	if o.Partial {
		// Never trade a complete recording for a partial one (--redo with
		// --keep-partial when the new take falls short).
		if cur := c.match(t); cur != nil && cur.complete() && exists(c.Path(cur)) {
			if !o.KeepWAV {
				_ = os.Remove(wavPath)
			}
			return *cur, ErrKeptComplete
		}
	}
	e := c.Ensure(t)
	e.merge(t, true) // metadata captured while recording is authoritative
	id := e.RequestedID

	if err := os.MkdirAll(l.opts.WorkDir, dirPerm); err != nil {
		return *e, fmt.Errorf("create work folder: %w", err)
	}
	scratch, err := os.MkdirTemp(l.opts.WorkDir, "encode-")
	if err != nil {
		return *e, fmt.Errorf("create work folder: %w", err)
	}
	defer func() { _ = os.RemoveAll(scratch) }()

	m4a := filepath.Join(scratch, "out"+m4aExt)
	if encErr := l.opts.Encoder.Encode(ctx, wavPath, m4a); encErr != nil {
		return l.encodeFailed(c, id, wavPath, encErr)
	}
	data, err := os.ReadFile(filepath.Clean(m4a))
	if err != nil {
		return l.encodeFailed(c, id, wavPath, err)
	}
	hash, err := mp4tag.AudioSHA256(data)
	if err != nil {
		return l.encodeFailed(c, id, wavPath, err)
	}

	e = c.Entry(id)
	if e.ReleaseDate == "" && l.opts.ReleaseDate != nil {
		// Album and playlist pages do not date their songs: ask the song's
		// own page. Without an answer the file simply has no year.
		lctx, cancel := context.WithTimeout(ctx, releaseDateTimeout)
		if d, dErr := l.opts.ReleaseDate(lctx, id); dErr == nil {
			e.ReleaseDate = d
		}
		cancel()
	}
	tags := l.tags(c, e, hash, o.Partial)
	if o.Artwork && e.ArtworkURL != "" {
		if cover, artErr := l.fetchArtwork(ctx, e.ArtworkURL); artErr != nil {
			l.warn("no artwork for %s: %v", e.Title, artErr)
		} else {
			tags.Cover = cover
		}
	}
	tagged, err := mp4tag.Tag(data, tags)
	if errors.Is(err, mp4tag.ErrCoverFormat) {
		l.warn("no artwork for %s: %v", e.Title, err)
		tags.Cover = nil
		tagged, err = mp4tag.Tag(data, tags)
	}
	if err != nil {
		return *e, fmt.Errorf("tag %s: %w", e.Title, err)
	}

	name := c.fileName(e)
	if err := fileutil.WriteAtomic(filepath.Join(c.Dir, name), tagged, audioPerm); err != nil {
		if !o.KeepWAV {
			_ = os.Remove(wavPath) // nothing will reference it: do not leak it
		}
		return *e, fmt.Errorf("%w: %s: %w", ErrStore, name, err)
	}

	now := l.opts.Now().UTC().Round(0)
	e.File = name
	e.Status = StatusRecorded
	e.Partial = o.Partial
	e.AudioSHA256 = hash
	e.RecordedAt = &now
	e.RecordedMS = o.RecordedMS
	e.CopiedFrom, e.FailReason, e.PendingWAV = "", "", ""
	result := *e
	saveErr := l.Save(c) // the file is in place either way: the next run adopts it by its tags
	if !o.KeepWAV {
		_ = os.Remove(wavPath)
	}
	if saveErr != nil {
		return result, fmt.Errorf("%w: %w", ErrStore, saveErr)
	}
	return result, nil
}

// RetryEncode encodes the WAV an earlier encode_failed Commit kept, so the
// song is stored without being recorded again.
func (l *Library) RetryEncode(ctx context.Context, c *Collection, id string, o CommitOptions) (Entry, error) {
	e := c.Entry(id)
	if e == nil {
		return Entry{}, fmt.Errorf("%w: %s in %s", errNoEntry, id, c.Folder())
	}
	if e.Status != StatusEncodeFailed || e.PendingWAV == "" {
		return *e, fmt.Errorf("%w: %s", errNoPendingWAV, id)
	}
	if _, err := os.Stat(e.PendingWAV); err != nil {
		return *e, fmt.Errorf("%w: %w", errNoPendingWAV, err)
	}
	return l.Commit(ctx, c, e.track(), e.PendingWAV, o)
}

// encodeFailed keeps the WAV for a retry and records the failure.
func (l *Library) encodeFailed(c *Collection, id, wavPath string, cause error) (Entry, error) {
	kept := wavPath
	failedDir := filepath.Join(l.opts.WorkDir, "failed")
	if filepath.Dir(wavPath) != failedDir {
		dst := filepath.Join(failedDir, c.Folder()+"-"+id+".wav")
		if err := moveFile(wavPath, dst); err != nil {
			l.warn("could not keep %s for a retry: %v", wavPath, err)
		} else {
			kept = dst
		}
	}
	e := c.Entry(id)
	e.Status = StatusEncodeFailed
	e.PendingWAV = kept
	e.FailReason = capBytes(cause.Error(), failReasonMax)
	result := *e
	if err := l.Save(c); err != nil {
		return result, errors.Join(fmt.Errorf("%w: %w", ErrEncode, cause), err)
	}
	return result, fmt.Errorf("%w: %w", ErrEncode, cause)
}

// Copy stores the song at src as t's file in c, retagged for c (grouping,
// album track numbers). The audio, and so its hash, is unchanged. The
// manifest and playlist are saved before it returns.
func (l *Library) Copy(_ context.Context, c *Collection, t Track, src Location) (Entry, error) {
	if t.ID == "" {
		return Entry{}, fmt.Errorf("%w: track without an ID", errSpec)
	}
	data, err := os.ReadFile(filepath.Clean(src.Path))
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}
	old, err := mp4tag.Parse(data)
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}
	hash, err := mp4tag.AudioSHA256(data)
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}

	e := c.Ensure(t)
	e.merge(t, true)
	e.merge(src.Entry.track(), false) // the source was recorded with richer metadata
	if played := src.Entry.playedOrRequested(); played != e.RequestedID {
		e.PlayedID = played
	}
	id := e.RequestedID

	tags := l.tags(c, e, hash, false)
	tags.Cover = old.Cover
	tagged, err := mp4tag.Tag(data, tags)
	if err != nil {
		return *e, fmt.Errorf("tag %s: %w", e.Title, err)
	}
	name := c.fileName(e)
	if err := fileutil.WriteAtomic(filepath.Join(c.Dir, name), tagged, audioPerm); err != nil {
		return *e, fmt.Errorf("%w: %s: %w", ErrStore, name, err)
	}

	e = c.Entry(id)
	e.File = name
	e.Status = StatusCopied
	e.Partial = false
	e.AudioSHA256 = hash
	e.CopiedFrom = l.rel(src.Path)
	e.RecordedMS = src.Entry.RecordedMS
	e.RecordedAt = src.Entry.RecordedAt
	if e.RecordedAt == nil {
		now := l.opts.Now().UTC().Round(0)
		e.RecordedAt = &now
	}
	e.FailReason, e.PendingWAV = "", ""
	result := *e
	if err := l.Save(c); err != nil {
		return result, fmt.Errorf("%w: %w", ErrStore, err)
	}
	return result, nil
}

// tags returns the full tag set for e in c.
func (l *Library) tags(c *Collection, e *Entry, hash string, partial bool) mp4tag.Tags {
	id := e.playedOrRequested()
	t := mp4tag.Tags{
		Title:       e.Title,
		Artist:      e.Artist,
		AlbumArtist: e.AlbumArtist,
		Album:       e.Album,
		Grouping:    c.Manifest.Name,
		Comment:     "https://open.spotify.com/track/" + id,
		TrackNumber: e.TrackNumber,
		DiscNumber:  e.DiscNumber,
		Date:        e.ReleaseDate,
		Freeform: map[string]string{
			TagSpotifyID:   id,
			TagRequestedID: "",
			TagAudioSHA256: hash,
			TagPartial:     "",
		},
	}
	if id != e.RequestedID {
		t.Freeform[TagRequestedID] = e.RequestedID
	}
	if partial {
		t.Freeform[TagPartial] = "1"
	}
	if c.Manifest.Kind == KindAlbum && e.Position > 0 {
		t.TrackNumber, t.TrackTotal = e.Position, c.activeCount()
	}
	return t
}

func (l *Library) fetchArtwork(ctx context.Context, url string) ([]byte, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, errArtworkURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.opts.HTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %d", errArtworkStatus, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxArtworkBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxArtworkBytes {
		return nil, errArtworkSize
	}
	return data, nil
}

// moveFile renames src to dst, copying across filesystems when needed.
func moveFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), dirPerm); err != nil {
		return err
	}
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	if err := fileutil.CopyFile(src, dst, 0o600); err != nil {
		return err
	}
	return os.Remove(src)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
