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
	"github.com/mrz1836/mixtape/internal/spotify"
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

// Encoder turns a WAV file into an M4A file.
type Encoder interface {
	Encode(ctx context.Context, wavPath, m4aPath string) error
}

// AFConvert encodes with macOS's built-in afconvert. The zero value
// encodes AAC at 256 kb/s.
type AFConvert struct {
	// Codec is what to encode to; "" means AAC.
	Codec mp4tag.Codec
	// Bitrate is the AAC bit rate in bits per second; 0 means 256000.
	// ALAC has none: it keeps every sample, at the WAV's own depth.
	Bitrate int
	// Warn receives a note when afconvert would not take Bitrate and the
	// song was encoded at its default instead; nil discards it.
	Warn func(msg string)

	run func(ctx context.Context, args ...string) error // nil: afconvert
}

// Encode runs afconvert. ALAC is `-f m4af -d alac`: lossless, at the bit
// depth of the WAV. AAC is `-f m4af -d aac -b <bitrate>`; if afconvert
// rejects the bitrate (some low sample rates cannot carry it), Encode
// retries once at afconvert's default bitrate and says so through Warn.
func (a AFConvert) Encode(ctx context.Context, wavPath, m4aPath string) error {
	run := a.run
	if run == nil {
		run = afconvert
	}
	if a.Codec == mp4tag.CodecALAC {
		if err := run(ctx, "-f", "m4af", "-d", "alac", wavPath, m4aPath); err != nil {
			_ = os.Remove(m4aPath)
			return err
		}
		return checkOutput(m4aPath)
	}
	bitrate := a.Bitrate
	if bitrate <= 0 {
		bitrate = defaultBitrate
	}
	err := run(ctx, "-f", "m4af", "-d", "aac", "-b", strconv.Itoa(bitrate), wavPath, m4aPath)
	if err != nil {
		_ = os.Remove(m4aPath)
		if retryErr := run(ctx, "-f", "m4af", "-d", "aac", wavPath, m4aPath); retryErr != nil {
			_ = os.Remove(m4aPath)
			return err
		}
		if a.Warn != nil {
			a.Warn(fmt.Sprintf("afconvert would not encode AAC at %d kb/s (%v); %s was encoded at its default bit rate",
				bitrate/1000, err, filepath.Base(wavPath)))
		}
	}
	return checkOutput(m4aPath)
}

// checkOutput makes sure the encoder left a non-empty file at path.
func checkOutput(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("afconvert output: %w", err)
	}
	if info.Size() == 0 {
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
// If encoding or tagging fails the WAV moves to WorkDir/failed, the entry
// becomes encode_failed with PendingWAV set (so RetryEncode can finish the
// job without recording again), and the error wraps ErrEncode.
func (l *Library) Commit(ctx context.Context, c *Collection, t Track, wavPath string, o CommitOptions) (Entry, error) {
	if err := requireID(t); err != nil {
		return Entry{}, err
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

	scratch, err := l.scratchDir()
	if err != nil {
		return *e, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	m4a := filepath.Join(scratch, "out"+m4aExt)
	enc, err := l.encode(ctx, wavPath, m4a)
	if err != nil {
		return l.encodeFailed(c, id, wavPath, err)
	}

	e = c.Entry(id)
	l.lookUpReleaseDate(ctx, e, id)
	tagged, err := l.tagRecording(ctx, c, e, m4a, enc.hash, o)
	if err != nil {
		// The audio is fine but cannot be stored yet: keep the WAV, like a
		// failed encode, so the next run finishes it without recording.
		return l.encodeFailed(c, id, wavPath, fmt.Errorf("tag: %w", err))
	}
	defer func() { _ = tagged.Close() }()

	result, err := l.finalize(c, e, tagged, stored{
		status: StatusRecorded, hash: enc.hash, codec: enc.codec, partial: o.Partial, recordedMS: o.RecordedMS,
	})
	if !o.KeepWAV && result.PendingWAV != wavPath {
		// Stored, or a fresh take the folder cannot take: nothing refers to
		// it any more, so it must not leak. A kept WAV the entry still
		// points at (a retry whose write failed) is the only copy of the
		// recording: it stays for the next retry.
		_ = os.Remove(wavPath)
	}
	return result, err
}

// requireID rejects a track without an ID, which Commit and Copy cannot
// store: the ID is what identifies its entry and its file.
func requireID(t Track) error {
	if t.ID == "" {
		return fmt.Errorf("%w: track without an ID", errSpec)
	}
	return nil
}

// scratchDir creates a fresh folder under WorkDir for one encode.
func (l *Library) scratchDir() (string, error) {
	if err := os.MkdirAll(l.opts.WorkDir, dirPerm); err != nil {
		return "", fmt.Errorf("create work folder: %w", err)
	}
	dir, err := os.MkdirTemp(l.opts.WorkDir, "encode-")
	if err != nil {
		return "", fmt.Errorf("create work folder: %w", err)
	}
	return dir, nil
}

// encoded describes a freshly encoded M4A.
type encoded struct {
	hash  string       // SHA-256 of the audio payload
	codec mp4tag.Codec // as the file declares it
}

// encode encodes the WAV at wavPath into m4aPath and reads back the hash of
// its audio payload and its codec, streaming the file rather than loading
// it.
func (l *Library) encode(ctx context.Context, wavPath, m4aPath string) (encoded, error) {
	if err := l.opts.Encoder.Encode(ctx, wavPath, m4aPath); err != nil {
		return encoded{}, err
	}
	hash, err := mp4tag.HashFile(m4aPath)
	if err != nil {
		return encoded{}, err
	}
	codec, err := mp4tag.ReadCodec(m4aPath)
	if err != nil {
		return encoded{}, err
	}
	return encoded{hash: hash, codec: codec}, nil
}

// lookUpReleaseDate asks the song's own page for e's release date when e
// has none. Album and playlist pages do not date their songs; without an
// answer the file simply has no year.
func (l *Library) lookUpReleaseDate(ctx context.Context, e *Entry, id string) {
	if e.ReleaseDate != "" || l.opts.ReleaseDate == nil {
		return
	}
	lctx, cancel := context.WithTimeout(ctx, releaseDateTimeout)
	defer cancel()
	if d, err := l.opts.ReleaseDate(lctx, id); err == nil {
		e.ReleaseDate = d
	}
}

// tagRecording prepares the encoded M4A at path, tagged for e in c, with
// its cover art when o.Artwork is set. Cover art is never worth losing a
// recording over: a cover that cannot be downloaded, or is in a format MP4
// cannot hold, is a warning and the file is tagged without it.
func (l *Library) tagRecording(ctx context.Context, c *Collection, e *Entry, path, hash string, o CommitOptions) (*mp4tag.Retagged, error) {
	tags := l.tags(c, e, hash, o.Partial)
	if o.Artwork && e.ArtworkURL != "" {
		if cover, err := l.fetchArtwork(ctx, e.ArtworkURL); err != nil {
			l.warn("no artwork for %s: %v", e.Title, err)
		} else {
			tags.Cover = cover
		}
	}
	tagged, err := mp4tag.TagFile(path, tags)
	if errors.Is(err, mp4tag.ErrCoverFormat) {
		l.warn("no artwork for %s: %v", e.Title, err)
		tags.Cover = nil
		tagged, err = mp4tag.TagFile(path, tags)
	}
	return tagged, err
}

// stored is what finalize records in an entry once its file is in place.
type stored struct {
	status     Status
	hash       string       // SHA-256 of the audio payload
	codec      mp4tag.Codec // of the file
	partial    bool
	copiedFrom string     // library-relative path of a copy's source
	recordedMS int64      // length of the captured audio
	recordedAt *time.Time // nil means now
}

// finalize streams tagged atomically into place as e's file in c, records
// s in e, and saves c's manifest and playlist. It returns the entry as
// stored.
//
// A write failure wraps ErrStore and leaves e as it was. A manifest that
// cannot be saved also wraps ErrStore, but the file is in place by then
// and the entry is updated in memory: the next run adopts the file by its
// tags.
func (l *Library) finalize(c *Collection, e *Entry, tagged io.WriterTo, s stored) (Entry, error) {
	name := c.fileName(e)
	if err := fileutil.WriteAtomicFrom(filepath.Join(c.Dir, name), tagged, audioPerm); err != nil {
		return *e, fmt.Errorf("%w: %s: %w", ErrStore, name, err)
	}
	e.File, e.Status, e.Partial, e.AudioSHA256, e.Codec = name, s.status, s.partial, s.hash, s.codec
	e.CopiedFrom, e.RecordedMS, e.RecordedAt = s.copiedFrom, s.recordedMS, s.recordedAt
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
// album track numbers). The audio, and so its hash and codec, is
// unchanged. The manifest and playlist are saved before it returns.
func (l *Library) Copy(_ context.Context, c *Collection, t Track, src Location) (Entry, error) {
	if err := requireID(t); err != nil {
		return Entry{}, err
	}
	old, err := mp4tag.ReadFile(src.Path)
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}
	hash, err := mp4tag.HashFile(src.Path)
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}
	codec, err := mp4tag.ReadCodec(src.Path)
	if err != nil {
		return Entry{}, fmt.Errorf("copy %s: %w", src.Path, err)
	}

	e := c.Ensure(t)
	e.merge(t, true)
	e.merge(src.Entry.track(), false) // the source was recorded with richer metadata
	if played := src.Entry.playedOrRequested(); played != e.RequestedID {
		e.PlayedID = played
	}
	tags := l.tags(c, e, hash, false)
	tags.Cover = old.Cover
	tagged, err := mp4tag.TagFile(src.Path, tags)
	if err != nil {
		return *e, fmt.Errorf("tag %s: %w", e.Title, err)
	}
	defer func() { _ = tagged.Close() }()
	return l.finalize(c, e, tagged, stored{
		status: StatusCopied, hash: hash, codec: codec, copiedFrom: l.rel(src.Path),
		recordedMS: src.Entry.RecordedMS, recordedAt: src.Entry.RecordedAt,
	})
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
		Comment:     spotify.TrackURL(id),
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
