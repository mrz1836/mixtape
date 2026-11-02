// Package spotify talks to the Spotify desktop app and to Spotify's public
// web pages, with no credentials and no API keys.
//
//   - Player drives and watches the app over AppleScript (osascript).
//   - Catalog resolves a link to its name and track list from the public
//     embed page, falling back to the oEmbed endpoint for the name alone.
//   - ParseRef turns any pasted link or URI into a validated Ref.
//   - ReadPrefs reads the playback preferences that matter for recording
//     (crossfade).
//
// Every ID that reaches AppleScript source is validated base62 first, so a
// crafted link can never inject script.
package spotify

import (
	"errors"
	"time"
)

// BundleID identifies the Spotify desktop app to macOS: the target of the
// Automation permission, and the prefix of the bundle IDs of the helper
// processes that play its audio.
const BundleID = "com.spotify.client"

// Kind is the type of Spotify item a link points at.
type Kind string

// Supported link kinds.
const (
	KindTrack    Kind = "track"
	KindAlbum    Kind = "album"
	KindPlaylist Kind = "playlist"
)

// Ref identifies one Spotify track, album or playlist.
type Ref struct {
	Kind Kind
	ID   string // base62
}

// State is the player state Spotify reports.
type State string

// Player states.
const (
	StatePlaying State = "playing"
	StatePaused  State = "paused"
	StateStopped State = "stopped"
)

// Track is the item loaded in the player, as Spotify's AppleScript
// dictionary describes it.
type Track struct {
	// ID is the base62 track ID, or "" when the item is not a track (an
	// ad, a local file) or Spotify did not report one.
	ID string
	// URI is the raw "id of current track": spotify:track:…, spotify:ad:…,
	// spotify:local:…, or "".
	URI         string
	Name        string
	Artist      string
	Album       string
	AlbumArtist string
	Duration    time.Duration
	TrackNumber int
	DiscNumber  int
	Popularity  int
	ArtworkURL  string
}

// Snapshot is one observation of the player.
type Snapshot struct {
	// At is when Spotify's position was sampled, on the monotonic clock.
	At time.Time
	// State, Position, Track, Volume, Shuffle and Repeat are valid only
	// when Err is nil.
	State State
	// Position is seconds into the track; negative when not reported.
	Position float64
	// Track is zero when nothing is loaded.
	Track   Track
	Volume  int // 0–100
	Shuffle bool
	Repeat  bool
	// Err is non-nil when the observation failed (Spotify not running,
	// permission denied, a transient AppleScript error).
	Err error
}

// CatalogTrack is one track of a resolved collection.
type CatalogTrack struct {
	ID       string
	Title    string
	Artist   string
	Duration time.Duration // 0 when unknown (oEmbed fallback)
	// Playable is the embed page's isPlayable flag. It reflects the market
	// of the server that rendered the page, so it is only a hint.
	Playable bool
	Position int // 1-based order in the collection
	// ReleaseDate is YYYY-MM-DD when the page names it (track pages do;
	// album and playlist pages do not).
	ReleaseDate string
}

// Collection is a resolved track, album or playlist.
type Collection struct {
	Ref        Ref
	Name       string
	Owner      string // playlist owner or album artist; "" for a track
	ArtworkURL string
	Tracks     []CatalogTrack
	// Truncated is true when the embed page returned its 100-track cap for
	// a playlist, so more tracks may exist.
	Truncated bool
	// Complete is true when Tracks is the whole collection.
	Complete bool
	// Source is "embed" or "oembed" (name only; Tracks may be empty).
	Source string
}

// Prefs are the Spotify playback preferences that affect recording.
type Prefs struct {
	// Path is the prefs file that was read, or "" when none was found.
	Path             string
	Crossfade        bool
	CrossfadeSeconds float64
	Automix          bool
	// Stale is set when Spotify has moved its crossfade setting out of the
	// prefs file (audio.crossfade_migrated_to_ps=true): the values above
	// are then leftovers that no longer follow the app's settings.
	Stale bool
}

// Errors returned by this package. Callers branch with errors.Is.
var (
	// ErrBadLink reports text that is not a Spotify link or URI.
	ErrBadLink = errors.New("not a Spotify track, album or playlist link")
	// ErrUnsupportedLink reports a valid Spotify link of a kind Mixtape
	// cannot record (episode, show, artist, local file).
	ErrUnsupportedLink = errors.New("unsupported Spotify link")
	// ErrNotFound reports a link Spotify does not know.
	ErrNotFound = errors.New("not found on Spotify")
	// ErrCatalog reports a catalog lookup that failed for another reason.
	ErrCatalog = errors.New("spotify catalog lookup failed")

	// ErrNotInstalled reports that the Spotify app is not installed.
	ErrNotInstalled = errors.New("the Spotify app is not installed")
	// ErrNotRunning reports that the Spotify app is not running.
	ErrNotRunning = errors.New("the Spotify app is not running")
	// ErrNotAuthorized reports that macOS denied Automation access to
	// Spotify (AppleScript error -1743).
	ErrNotAuthorized = errors.New("not allowed to control Spotify (Automation permission)")
	// ErrAppleEventsBlocked reports that Apple Events fail although
	// Spotify is running, as happens inside a sandbox.
	ErrAppleEventsBlocked = errors.New("apple events to Spotify are blocked")
	// ErrPromptPending reports an AppleScript call that timed out, which
	// on first use means macOS is showing the Automation prompt.
	ErrPromptPending = errors.New("waiting for the Automation permission prompt")
	// ErrScript reports any other AppleScript failure.
	ErrScript = errors.New("AppleScript call to Spotify failed")
)
