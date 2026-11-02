// Package library owns the folder of recordings: one folder per playlist,
// album or singles collection, each with a mixtape.json manifest, a
// regenerated <Folder>.m3u8, and one tagged M4A per song.
//
// Identity is the Spotify track ID, both the requested one and the one
// actually played (Spotify relinks tracks between releases). Every file
// also carries the SHA-256 of its AAC audio payload, which tagging never
// changes, so files describe themselves even when moved or renamed.
package library

import (
	"errors"
	"time"
)

// Schema is the manifest schema version this package writes.
const Schema = 1

// ManifestName is the manifest file inside every collection folder.
const ManifestName = "mixtape.json"

// Kind is the type of collection a folder holds.
type Kind string

// Collection kinds.
const (
	KindPlaylist Kind = "playlist"
	KindAlbum    Kind = "album"
	KindSingles  Kind = "singles" // individual tracks and live-mode recordings
	KindFolder   Kind = "folder"  // a user-named folder (--into)
)

// Status is the state of one track in a manifest.
type Status string

// Track statuses.
const (
	StatusPending      Status = "pending"
	StatusRecorded     Status = "recorded"
	StatusCopied       Status = "copied"
	StatusUnavailable  Status = "unavailable"
	StatusFailed       Status = "failed"
	StatusEncodeFailed Status = "encode_failed"
	StatusRemoved      Status = "removed" // no longer in the collection; the file is kept
)

// Done reports whether the status means the song is in the folder.
func (s Status) Done() bool { return s == StatusRecorded || s == StatusCopied }

// Track is the metadata the library records for a song.
type Track struct {
	ID          string // requested track ID: the one in the collection
	PlayedID    string // the ID Spotify actually played, when relinked
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	DurationMS  int64
	TrackNumber int
	DiscNumber  int
	Popularity  int
	ArtworkURL  string
}

// Entry is one track in a manifest.
type Entry struct {
	Position    int        `json:"position"`
	RequestedID string     `json:"requested_id"`
	PlayedID    string     `json:"played_id,omitempty"`
	Title       string     `json:"title"`
	Artist      string     `json:"artist"`
	Album       string     `json:"album,omitempty"`
	AlbumArtist string     `json:"album_artist,omitempty"`
	DurationMS  int64      `json:"duration_ms,omitempty"`
	Status      Status     `json:"status"`
	File        string     `json:"file,omitempty"`
	RecordedMS  int64      `json:"recorded_ms,omitempty"`
	AudioSHA256 string     `json:"audio_sha256,omitempty"`
	RecordedAt  *time.Time `json:"recorded_at,omitempty"`
	CopiedFrom  string     `json:"copied_from,omitempty"`
	ArtworkURL  string     `json:"artwork_url,omitempty"`
	TrackNumber int        `json:"track_number,omitempty"`
	DiscNumber  int        `json:"disc_number,omitempty"`
	Popularity  int        `json:"popularity,omitempty"`
	Partial     bool       `json:"partial,omitempty"`
	FailReason  string     `json:"fail_reason,omitempty"`
	PendingWAV  string     `json:"pending_wav,omitempty"`
}

// Manifest is the mixtape.json document of one collection folder.
type Manifest struct {
	Schema    int       `json:"schema"`
	Kind      Kind      `json:"kind"`
	SpotifyID string    `json:"spotify_id,omitempty"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner,omitempty"`
	URL       string    `json:"url,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Tracks    []Entry   `json:"tracks"`
}

// Errors returned by this package.
var (
	// ErrLocked reports that another mixtape process is writing the library.
	ErrLocked = errors.New("the library is locked by another mixtape process")
	// ErrEncode reports that afconvert could not encode a recording. The
	// WAV is kept and the next run retries it.
	ErrEncode = errors.New("encoding to M4A failed")
	// ErrUnsafeFix reports a `verify --fix` refused because the library
	// looks unmounted or mostly missing.
	ErrUnsafeFix = errors.New("refusing to rewrite manifests: the library looks unmounted or mostly missing")
	// ErrBadManifest reports a mixtape.json that cannot be read.
	ErrBadManifest = errors.New("unreadable manifest")
)
