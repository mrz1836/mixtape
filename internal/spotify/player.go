package spotify

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Line markers the scripts emit.
const (
	markSnapshot   = "S" // a snapshot follows
	markNotRunning = "N" // Spotify is not running: nothing was queried
	markError      = "E" // an AppleScript error: number, then message
	markOK         = "OK"
	fieldSep       = "\x1f" // ASCII unit separator, `character id 31`
)

// Snapshot field order after the marker. Position is last so it is
// sampled as close as possible to the moment the line is emitted.
const (
	fState = iota
	fVolume
	fShuffle
	fRepeat
	fID
	fName
	fArtist
	fAlbum
	fAlbumArtist
	fDuration
	fTrackNumber
	fDiscNumber
	fPopularity
	fArtwork
	fPosition
	fieldCount
)

// snapshotHandler is the AppleScript handler both the one-shot and the
// streaming scripts call. It never launches Spotify: every query sits
// behind `application "Spotify" is running`. Track reads are wrapped in
// their own try so a stopped player with nothing loaded still reports.
const snapshotHandler = `
on mixtapeClean(t)
	-- One line per snapshot: no line breaks or separators inside a field,
	-- and "missing value" (a podcast, a local file) reads as empty.
	if t is missing value then return ""
	set t to t as text
	set saved to AppleScript's text item delimiters
	repeat with c in {linefeed, return, character id 31}
		set AppleScript's text item delimiters to (contents of c)
		set parts to text items of t
		set AppleScript's text item delimiters to " "
		set t to parts as text
	end repeat
	set AppleScript's text item delimiters to saved
	return t
end mixtapeClean

on mixtapeSnapshot()
	set US to character id 31
	if application "Spotify" is not running then return "N"
	try
		tell application "Spotify"
			set mState to player state as string
			set mVolume to sound volume
			set mShuffle to shuffling
			set mRepeat to repeating
			set mID to ""
			set mName to ""
			set mArtist to ""
			set mAlbum to ""
			set mAlbumArtist to ""
			set mDuration to 0
			set mTrackNumber to 0
			set mDiscNumber to 0
			set mPopularity to 0
			set mArtwork to ""
			try
				set mTrack to current track
				set mID to my mixtapeClean(id of mTrack)
				set mName to my mixtapeClean(name of mTrack)
				set mArtist to my mixtapeClean(artist of mTrack)
				set mAlbum to my mixtapeClean(album of mTrack)
				set mAlbumArtist to my mixtapeClean(album artist of mTrack)
				set mDuration to duration of mTrack
				set mTrackNumber to track number of mTrack
				set mDiscNumber to disc number of mTrack
				set mPopularity to popularity of mTrack
				set mArtwork to my mixtapeClean(artwork url of mTrack)
			end try
			set mPosition to player position
		end tell
		return "S" & US & mState & US & mVolume & US & mShuffle & US & mRepeat & US & mID & US & mName & US & mArtist & US & mAlbum & US & mAlbumArtist & US & mDuration & US & mTrackNumber & US & mDiscNumber & US & mPopularity & US & mArtwork & US & mPosition
	on error errMsg number errNum
		return "E" & US & errNum & US & errMsg
	end try
end mixtapeSnapshot
`

// pollScript takes one snapshot.
const pollScript = snapshotHandler + `
on run argv
	return mixtapeSnapshot()
end run
`

// watchScript logs a snapshot every argv[1] milliseconds until killed.
const watchScript = snapshotHandler + `
on run argv
	set interval to ((item 1 of argv) as integer) / 1000
	repeat
		log mixtapeSnapshot()
		delay interval
	end repeat
end run
`

// commandScript runs one Spotify command, refusing to launch the app.
// The %s is a fixed command built by this package from validated values.
const commandScript = `
on run argv
	if application "Spotify" is not running then return "N"
	tell application "Spotify"
		%s
	end tell
	return "OK"
end run
`

// Watch tuning.
const (
	minWatchInterval = 20 * time.Millisecond
	watchBackoffMin  = time.Second
	watchBackoffMax  = 5 * time.Second
)

// maxDurationMS is the longest track duration, in milliseconds, that a
// time.Duration can hold (about 292 years). A longer one would wrap.
const maxDurationMS = float64(math.MaxInt64 / int64(time.Millisecond))

// errBadSnapshot reports a script line Mixtape cannot parse.
var errBadSnapshot = errors.New("unexpected snapshot output")

// errBadURI reports a URI PlayTrack refuses to splice into AppleScript.
var errBadURI = errors.New("invalid Spotify URI")

// errBadValue reports a non-finite or negative position.
var errBadValue = errors.New("invalid value")

// Player drives and observes the Spotify desktop app over AppleScript.
// It is safe for concurrent use.
type Player struct {
	run        Runner
	isRunning  func() (bool, error)
	now        func() time.Time
	backoffMin time.Duration
	backoffMax time.Duration
}

// NewPlayer returns a Player that runs scripts with r (nil means the real
// osascript).
func NewPlayer(r Runner) *Player {
	if r == nil {
		r = &OSAScript{}
	}
	return &Player{
		run:        r,
		isRunning:  IsRunning,
		now:        time.Now,
		backoffMin: watchBackoffMin,
		backoffMax: watchBackoffMax,
	}
}

// Poll takes one snapshot of the player. A failure is reported in
// Snapshot.Err, never as a panic or a zero snapshot without an error.
func (p *Player) Poll(ctx context.Context) Snapshot {
	out, err := p.run.Run(ctx, pollScript)
	at := p.now()
	if err != nil {
		return Snapshot{At: at, Err: p.classify(err)}
	}
	return p.parseSnapshot(out, at)
}

// Watch streams snapshots about every interval from one long-running
// osascript process, which costs far less than spawning a process per
// poll. If the process dies it is restarted with backoff, and a snapshot
// carrying the error is delivered. The channel closes when ctx is done.
func (p *Player) Watch(ctx context.Context, interval time.Duration) <-chan Snapshot {
	out := make(chan Snapshot, 8)
	go p.watch(ctx, max(interval, minWatchInterval), out)
	return out
}

func (p *Player) watch(ctx context.Context, interval time.Duration, out chan<- Snapshot) {
	defer close(out)
	send := func(s Snapshot) bool {
		select {
		case out <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}
	ms := strconv.FormatInt(interval.Milliseconds(), 10)
	backoff := p.backoffMin
	for ctx.Err() == nil {
		lines, err := p.run.Start(ctx, watchScript, ms)
		if err != nil {
			if !send(Snapshot{At: p.now(), Err: p.classify(err)}) {
				return
			}
		} else {
			healthy := false
			for l := range lines {
				if l.Err != nil {
					if ctx.Err() != nil {
						return
					}
					if !send(Snapshot{At: l.At, Err: p.classify(l.Err)}) {
						return
					}
					continue
				}
				if !isSnapshotLine(l.Text) {
					continue // framework noise on stderr (NSLog lines), not ours
				}
				healthy = true
				if !send(p.parseSnapshot(l.Text, l.At)) {
					return
				}
			}
			if healthy {
				backoff = p.backoffMin
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, p.backoffMax)
	}
}

// PlayTrack starts trackURI, inside contextURI (an album or playlist URI)
// when it is not empty, so the following tracks of that context play on.
// Both URIs are validated before they reach AppleScript.
func (p *Player) PlayTrack(ctx context.Context, trackURI, contextURI string) error {
	if !safeURI(trackURI) {
		return fmt.Errorf("%w: %q", errBadURI, trackURI)
	}
	cmd := `play track "` + trackURI + `"`
	if contextURI != "" {
		if !safeURI(contextURI) {
			return fmt.Errorf("%w: %q", errBadURI, contextURI)
		}
		cmd += ` in context "` + contextURI + `"`
	}
	return p.command(ctx, cmd)
}

// Play resumes playback.
func (p *Player) Play(ctx context.Context) error { return p.command(ctx, "play") }

// Pause pauses playback.
func (p *Player) Pause(ctx context.Context) error { return p.command(ctx, "pause") }

// Next skips to the next track.
func (p *Player) Next(ctx context.Context) error { return p.command(ctx, "next track") }

// PlayPause toggles playback.
func (p *Player) PlayPause(ctx context.Context) error { return p.command(ctx, "playpause") }

// SetPosition seeks within the current track.
func (p *Player) SetPosition(ctx context.Context, seconds float64) error {
	if math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds < 0 {
		return fmt.Errorf("%w: position %v", errBadValue, seconds)
	}
	return p.command(ctx, "set player position to "+strconv.FormatFloat(seconds, 'f', 3, 64))
}

// SetShuffle turns shuffle on or off.
func (p *Player) SetShuffle(ctx context.Context, on bool) error {
	return p.command(ctx, "set shuffling to "+strconv.FormatBool(on))
}

// SetRepeat turns repeat on or off.
func (p *Player) SetRepeat(ctx context.Context, on bool) error {
	return p.command(ctx, "set repeating to "+strconv.FormatBool(on))
}

// SetVolume sets Spotify's own volume, clamped to 0–100.
func (p *Player) SetVolume(ctx context.Context, volume int) error {
	return p.command(ctx, "set sound volume to "+strconv.Itoa(min(max(volume, 0), 100)))
}

// command runs one fixed Spotify command.
func (p *Player) command(ctx context.Context, cmd string) error {
	out, err := p.run.Run(ctx, fmt.Sprintf(commandScript, cmd))
	if err != nil {
		return p.classify(err)
	}
	switch out {
	case markOK:
		return nil
	case markNotRunning:
		return ErrNotRunning
	}
	return fmt.Errorf("%w: %s: %q", errBadSnapshot, cmd, out)
}

// classify maps a runner error to a sentinel, consulting the process list
// only when the error is ambiguous.
func (p *Player) classify(err error) error {
	running := false
	if needsProcessCheck(err) {
		running, _ = p.isRunning()
	}
	return Classify(err, running)
}

// parseSnapshot decodes one line of snapshot output.
func (p *Player) parseSnapshot(line string, at time.Time) Snapshot {
	snap := Snapshot{At: at, Position: -1}
	marker, rest, _ := strings.Cut(strings.TrimSpace(line), fieldSep)
	switch marker {
	case markNotRunning:
		snap.Err = ErrNotRunning
		return snap
	case markError:
		num, msg, _ := strings.Cut(rest, fieldSep)
		code, err := strconv.Atoi(strings.TrimSpace(num))
		if err != nil {
			snap.Err = fmt.Errorf("%w: %s", ErrScript, rest)
			return snap
		}
		detail := fmt.Sprintf("%s (%d)", msg, code)
		running := false
		if ambiguousCode(code) {
			running, _ = p.isRunning()
		}
		snap.Err = classifyCode(code, detail, running)
		return snap
	case markSnapshot:
	default:
		snap.Err = fmt.Errorf("%w: %q", errBadSnapshot, line)
		return snap
	}

	f := strings.Split(rest, fieldSep)
	if len(f) != fieldCount {
		snap.Err = fmt.Errorf("%w: %d fields, want %d", errBadSnapshot, len(f), fieldCount)
		return snap
	}
	snap.State = State(strings.TrimSpace(f[fState]))
	snap.Volume = atoi(f[fVolume])
	snap.Shuffle = f[fShuffle] == "true"
	snap.Repeat = f[fRepeat] == "true"
	if pos, ok := parseReal(f[fPosition]); ok {
		snap.Position = pos
	}
	uri := strings.TrimSpace(f[fID])
	snap.Track = Track{
		ID:          parseTrackURI(uri),
		URI:         uri,
		Name:        textField(f[fName]),
		Artist:      textField(f[fArtist]),
		Album:       textField(f[fAlbum]),
		AlbumArtist: textField(f[fAlbumArtist]),
		TrackNumber: atoi(f[fTrackNumber]),
		DiscNumber:  atoi(f[fDiscNumber]),
		Popularity:  atoi(f[fPopularity]),
		ArtworkURL:  strings.TrimSpace(f[fArtwork]),
	}
	if ms, ok := parseReal(f[fDuration]); ok && ms > 0 && ms < maxDurationMS {
		snap.Track.Duration = time.Duration(ms) * time.Millisecond
	}
	return snap
}

// textField is a text field as read: AppleScript's "missing value" (no
// value at all) becomes empty.
func textField(s string) string {
	if s == "missing value" {
		return ""
	}
	return s
}

// isSnapshotLine reports whether a streamed line is one the watch script
// emitted, as opposed to system log noise on the same stderr.
func isSnapshotLine(line string) bool {
	marker, _, _ := strings.Cut(strings.TrimSpace(line), fieldSep)
	return marker == markSnapshot || marker == markNotRunning || marker == markError
}

// IsAd reports whether the item is an advertisement rather than a song.
func (t Track) IsAd() bool {
	return strings.HasPrefix(t.URI, "spotify:ad:") || (t.ID == "" && strings.EqualFold(t.Name, "Advertisement"))
}

// safeURI reports whether uri is a track, album or playlist URI with a
// valid ID: the only shapes PlayTrack splices into AppleScript.
func safeURI(uri string) bool {
	parts := strings.Split(uri, ":")
	if len(parts) != 3 || parts[0] != "spotify" {
		return false
	}
	switch Kind(parts[1]) {
	case KindTrack, KindAlbum, KindPlaylist:
		return ValidID(parts[2])
	}
	return false
}

// parseReal parses an AppleScript number, which follows the user's locale
// (a comma decimal separator in many of them).
func parseReal(s string) (float64, bool) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	if s == "" || s == "missing value" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// atoi parses an integer field, tolerating a real ("3.0") and returning 0
// for anything unusable.
func atoi(s string) int {
	v, ok := parseReal(s)
	if !ok || v > math.MaxInt32 || v < math.MinInt32 {
		return 0
	}
	return int(v)
}
