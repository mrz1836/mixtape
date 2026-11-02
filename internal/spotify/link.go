package spotify

import (
	"fmt"
	"net/url"
	"strings"
)

// maxIDLen bounds an ID. Spotify IDs are 22 base62 characters; the limit
// only keeps absurd input out of AppleScript and URLs.
const maxIDLen = 64

// Hosts that serve Spotify share links.
const (
	hostOpen = "open.spotify.com"
	hostPlay = "play.spotify.com"
)

// URI returns the Spotify URI, e.g. "spotify:track:<id>".
func (r Ref) URI() string { return "spotify:" + string(r.Kind) + ":" + r.ID }

// URL returns the share URL, e.g. "https://open.spotify.com/track/<id>".
func (r Ref) URL() string { return "https://" + hostOpen + "/" + string(r.Kind) + "/" + r.ID }

// String returns the URI.
func (r Ref) String() string { return r.URI() }

// TrackURI returns the URI of the track with the given ID.
func TrackURI(id string) string { return Ref{Kind: KindTrack, ID: id}.URI() }

// TrackURL returns the share URL of the track with the given ID.
func TrackURL(id string) string { return Ref{Kind: KindTrack, ID: id}.URL() }

// ValidID reports whether id is a plausible Spotify ID: 1–64 characters
// of [0-9A-Za-z]. IDs end up inside AppleScript source and URLs, so
// nothing else may pass.
func ValidID(id string) bool {
	if id == "" || len(id) > maxIDLen {
		return false
	}
	for _, r := range id {
		if (r < '0' || r > '9') && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

// ParseRef parses a pasted Spotify link or URI into a Ref. It accepts:
//
//   - https://open.spotify.com/{track|album|playlist}/<id>, with or without
//     the scheme, share parameters (?si=…), a fragment, a trailing slash,
//     or a locale prefix such as /intl-de/; play.spotify.com links too;
//   - URIs: spotify:{track|album|playlist}:<id> and the legacy
//     spotify:user:<name>:playlist:<id>.
//
// Surrounding whitespace, quotes and angle brackets are ignored. Links to
// episodes, shows, artists, users and local files are ErrUnsupportedLink;
// anything else is ErrBadLink.
func ParseRef(s string) (Ref, error) {
	raw := strings.Trim(strings.TrimSpace(s), `<>"'`)
	if raw == "" {
		return Ref{}, fmt.Errorf("%w: empty", ErrBadLink)
	}
	if strings.HasPrefix(strings.ToLower(raw), "spotify:") {
		return parseURI(raw, s)
	}
	return parseURL(raw, s)
}

func parseURI(raw, orig string) (Ref, error) {
	parts := strings.Split(raw, ":")
	// spotify:user:<name>:playlist:<id> → spotify:playlist:<id>
	if len(parts) == 5 && strings.EqualFold(parts[1], "user") {
		parts = []string{parts[0], parts[3], parts[4]}
	}
	if len(parts) < 2 {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}
	kind := strings.ToLower(parts[1])
	if len(parts) != 3 {
		if isUnsupportedKind(kind) {
			return Ref{}, fmt.Errorf("%w: %s", ErrUnsupportedLink, kindName(kind))
		}
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}
	return makeRef(kind, parts[2], orig)
}

func parseURL(raw, orig string) (Ref, error) {
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}
	host := strings.ToLower(u.Hostname())
	if (u.Scheme != "https" && u.Scheme != "http") || (host != hostOpen && host != hostPlay) {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}

	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	if len(segs) > 0 && strings.HasPrefix(strings.ToLower(segs[0]), "intl-") {
		segs = segs[1:] // locale prefix: /intl-de/track/<id>
	}
	// /user/<name>/playlist/<id> (legacy share links)
	if len(segs) == 4 && strings.EqualFold(segs[0], "user") && strings.EqualFold(segs[2], "playlist") {
		segs = segs[2:]
	}
	if len(segs) == 0 {
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}
	kind := strings.ToLower(segs[0])
	if len(segs) != 2 {
		if isUnsupportedKind(kind) {
			return Ref{}, fmt.Errorf("%w: %s", ErrUnsupportedLink, kindName(kind))
		}
		return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
	}
	return makeRef(kind, segs[1], orig)
}

// makeRef validates a kind and ID pair.
func makeRef(kind, id, orig string) (Ref, error) {
	switch Kind(kind) {
	case KindTrack, KindAlbum, KindPlaylist:
		if !ValidID(id) {
			return Ref{}, fmt.Errorf("%w: invalid %s ID in %q", ErrBadLink, kind, orig)
		}
		return Ref{Kind: Kind(kind), ID: id}, nil
	}
	if isUnsupportedKind(kind) {
		return Ref{}, fmt.Errorf("%w: %s", ErrUnsupportedLink, kindName(kind))
	}
	return Ref{}, fmt.Errorf("%w: %q", ErrBadLink, orig)
}

// isUnsupportedKind reports Spotify item kinds that are real but cannot
// be recorded as songs.
func isUnsupportedKind(kind string) bool {
	switch kind {
	case "episode", "show", "artist", "local", "user", "ad", "concert", "audiobook", "chapter", "genre":
		return true
	}
	return false
}

// kindName renders an unsupported kind for an error message.
func kindName(kind string) string {
	switch kind {
	case "local":
		return "local files cannot be recorded"
	case "user":
		return "user profiles are not collections (link a playlist)"
	case "ad":
		return "ads cannot be recorded"
	}
	return kind + " links are not supported (link a track, album or playlist)"
}

// parseTrackURI returns the base62 ID of a spotify:track:<id> URI, or "".
func parseTrackURI(uri string) string {
	id, ok := strings.CutPrefix(uri, "spotify:track:")
	if !ok || !ValidID(id) {
		return ""
	}
	return id
}
