package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/wav"
)

// The simulation runs at a low sample rate to keep spools small.
const (
	testRate = 8000
	stepDur  = 10 * time.Millisecond
	pollDur  = 150 * time.Millisecond
	adPrefix = "spotify:ad:"
)

// simTrack is a song (or ad) the fake Spotify can play.
type simTrack struct {
	id, name, artist string
	dur              time.Duration
	ad               bool
}

// fakeSpotify simulates the desktop app: contexts that play through in
// order, repeat, ads, relinked and unplayable tracks, and the commands the
// engine sends. Time advances only through the driver.
type fakeSpotify struct {
	mu         sync.Mutex
	tracks     map[string]simTrack
	contexts   map[string][]string
	unplayable map[string]bool
	relink     map[string]string // requested ID → ID Spotify plays

	trace     []string
	queue     []string
	idx       int
	pos       time.Duration
	playing   bool
	crossfade time.Duration // change to the next track this long before the end
	// startGlitch replays Spotify's habit of starting a freshly played song
	// over about 0.2 s in; glitched is set once it happened for this play.
	startGlitch, glitched bool
	// stalePlay replays Spotify reporting the previous track's position
	// under a newly played track for a few observations.
	stalePlay bool
	staleFor  int
	stalePos  time.Duration
	// blipOnPlay replays Spotify reporting the next queued track for one
	// observation right after a song is played.
	blipOnPlay bool
	blipIn     int
	blipLen    int           // how many observations the blip lasts (default 1)
	blipPos    time.Duration // the position reported during the blip
	blipping   int
	// resume replays Spotify picking a song up where it was last left: the
	// first play of a track ID listed here jumps, a few observations in, to
	// the saved spot (resumeTrack, when set, at resumePos) and plays on.
	resume   map[string]resumeSpot
	resumeIn int
	resumeTo resumeSpot
	hold     time.Duration // position frozen at 0 while it re-buffers
	shuffle  bool
	repeat   bool
	volume   int
	commands []string

	now   func() time.Time
	watch chan spotify.Snapshot
	wdone <-chan struct{} // the watcher's context is done
}

// resumeSpot is where Spotify resumes: a track (the played one when "")
// and a position. Spotify holds the song at exactly 0:00 until it resumes,
// unless moving: then it first plays a moment from the top.
type resumeSpot struct {
	track  string
	pos    time.Duration
	moving bool
}

func newFakeSpotify(now func() time.Time, tracks ...simTrack) *fakeSpotify {
	f := &fakeSpotify{
		tracks: map[string]simTrack{}, contexts: map[string][]string{},
		unplayable: map[string]bool{}, relink: map[string]string{}, volume: 50, now: now,
	}
	for _, t := range tracks {
		f.tracks[t.id] = t
	}
	return f
}

func (f *fakeSpotify) current() (simTrack, bool) {
	if f.idx < 0 || f.idx >= len(f.queue) {
		return simTrack{}, false
	}
	t, ok := f.tracks[f.queue[f.idx]]
	return t, ok
}

// advance moves playback forward by d.
func (f *fakeSpotify) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.current()
	if !ok || !f.playing {
		return
	}
	if f.hold > 0 {
		f.hold -= d
		return
	}
	f.pos += d
	if f.startGlitch && !f.glitched && f.pos >= 300*time.Millisecond {
		f.glitched, f.pos, f.hold = true, 0, 400*time.Millisecond
		return
	}
	if f.pos < t.dur-f.crossfade {
		return
	}
	f.pos = 0
	f.idx++
	if f.idx >= len(f.queue) {
		if f.repeat && len(f.queue) > 0 {
			f.idx = 0
			return
		}
		f.idx = len(f.queue) - 1
		f.playing = false // the context ended: parked on the last track at 0:00
	}
}

func (f *fakeSpotify) snapshot() spotify.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := spotify.Snapshot{At: f.now(), State: spotify.StatePaused, Volume: f.volume, Shuffle: f.shuffle, Repeat: f.repeat}
	if f.playing {
		s.State = spotify.StatePlaying
	}
	if f.resumeIn > 0 {
		f.resumeIn--
		if f.resumeIn == 0 {
			if r := f.resumeTo; r.track != "" {
				f.queue, f.idx = []string{r.track}, 0
			}
			f.pos, f.hold = f.resumeTo.pos, 0
		}
	}
	t, ok := f.current()
	blip := false
	if f.blipIn > 0 {
		f.blipIn--
		if f.blipIn == 0 {
			f.blipping = max(f.blipLen, 1)
		}
	}
	if f.blipping > 0 && f.idx+1 < len(f.queue) {
		f.blipping--
		t, ok, blip = f.tracks[f.queue[f.idx+1]], true, true
	}
	if ok {
		s.Position = f.pos.Seconds()
		if blip && f.blipPos > 0 {
			s.Position = f.blipPos.Seconds()
		}
		if f.staleFor > 0 {
			f.staleFor--
			s.Position = f.stalePos.Seconds()
		}
		dur := t.dur
		if f.crossfade > 0 && f.pos > time.Second {
			dur -= f.crossfade // like Spotify: the duration shrinks once playback settles
		}
		s.Track = spotify.Track{
			ID: t.id, URI: spotify.TrackURI(t.id), Name: t.name, Artist: t.artist, Duration: dur,
			Album: "Album", AlbumArtist: t.artist, TrackNumber: f.idx + 1, DiscNumber: 1,
		}
		if t.ad {
			s.Track = spotify.Track{URI: adPrefix + t.id, Name: "Advertisement", Duration: t.dur}
		}
	}
	return s
}

func (f *fakeSpotify) log(cmd string) {
	f.commands = append(f.commands, cmd)
	f.trace = append(f.trace, f.traceLine(cmd))
}

// trace lists the commands with the simulated time and position they
// arrived at, for failure dumps.
func (f *fakeSpotify) traceLine(cmd string) string {
	return fmt.Sprintf("%s@%s[pos %.2f idx %d playing %v]", cmd, f.now().Format("05.00"), f.pos.Seconds(), f.idx, f.playing)
}

// Commands returns the commands received so far.
func (f *fakeSpotify) Commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.commands...)
}

func (f *fakeSpotify) Poll(context.Context) spotify.Snapshot { return f.snapshot() }

func (f *fakeSpotify) Watch(ctx context.Context, _ time.Duration) <-chan spotify.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watch, f.wdone = make(chan spotify.Snapshot), ctx.Done()
	return f.watch
}

func (f *fakeSpotify) PlayTrack(_ context.Context, uri, ctxURI string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("play " + uri + " in " + ctxURI)
	if f.stalePlay {
		f.stalePos, f.staleFor = f.pos+12*time.Second, 3
	}
	if f.blipOnPlay {
		f.blipIn = 3 // the third observation after play shows the next track
	}
	id := uri[strings.LastIndex(uri, ":")+1:]
	if r, ok := f.resume[id]; ok {
		delete(f.resume, id) // once played, the song's saved spot is its top
		f.resumeIn, f.resumeTo = 3, r
	}
	switch {
	case strings.HasPrefix(uri, "spotify:playlist:"), strings.HasPrefix(uri, "spotify:album:"):
		f.queue, f.idx = append([]string(nil), f.contexts[uri]...), 0
	case ctxURI != "":
		f.queue, f.idx = append([]string(nil), f.contexts[ctxURI]...), 0
		for i, q := range f.queue {
			if q == id {
				f.idx = i
			}
		}
	default:
		f.queue, f.idx = []string{id}, 0
	}
	for f.idx < len(f.queue) && f.unplayable[f.queue[f.idx]] {
		f.idx++ // Spotify jumps past what it cannot play
	}
	if f.idx < len(f.queue) {
		if to, ok := f.relink[f.queue[f.idx]]; ok {
			f.queue[f.idx] = to
		}
	}
	f.pos, f.playing = 0, f.idx < len(f.queue)
	f.glitched = false
	if f.resumeIn > 0 && !f.resumeTo.moving {
		f.hold = time.Hour // as seen live: exactly 0:00 until the resume
	}
	return nil
}

func (f *fakeSpotify) Play(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("play")
	f.playing = true
	return nil
}

func (f *fakeSpotify) Pause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("pause")
	f.playing = false
	return nil
}

func (f *fakeSpotify) PlayPause(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("playpause")
	f.playing = !f.playing
	return nil
}

func (f *fakeSpotify) Next(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("next")
	f.pos = 0
	f.idx++
	if f.idx >= len(f.queue) {
		f.idx = 0
		if !f.repeat {
			f.playing = false
		}
	}
	return nil
}

func (f *fakeSpotify) SetPosition(_ context.Context, sec float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("seek")
	f.pos = time.Duration(sec * float64(time.Second))
	return nil
}

func (f *fakeSpotify) SetShuffle(_ context.Context, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("shuffle")
	f.shuffle = on
	return nil
}

func (f *fakeSpotify) SetRepeat(_ context.Context, on bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("repeat")
	f.repeat = on
	return nil
}

func (f *fakeSpotify) SetVolume(_ context.Context, v int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log("volume")
	f.volume = v
	return nil
}

// user applies a change as if the listener did it in the Spotify app.
func (f *fakeSpotify) user(fn func(f *fakeSpotify)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

// fakeSource is a capture source the driver feeds.
type fakeSource struct {
	mu     sync.Mutex
	sink   func(capture.Block)
	events chan capture.Event
	silent bool
}

func newFakeSource() *fakeSource { return &fakeSource{events: make(chan capture.Event, 4)} }

func (s *fakeSource) Name() string                 { return "fake" }
func (s *fakeSource) Method() capture.Method       { return capture.MethodDevice }
func (s *fakeSource) Format() capture.Format       { return capture.Format{Rate: testRate, Channels: 2} }
func (s *fakeSource) Events() <-chan capture.Event { return s.events }
func (s *fakeSource) Close() error                 { return nil }

func (s *fakeSource) Start(_ context.Context, sink func(capture.Block)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sink = sink
	return nil
}

// emit delivers frames captured at t; loud frames carry a constant level.
func (s *fakeSource) emit(frames int, t time.Time, loud bool) {
	s.mu.Lock()
	sink, silent := s.sink, s.silent
	s.mu.Unlock()
	if sink == nil {
		return
	}
	pcm := make([]byte, frames*4)
	if loud && !silent {
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], 3000)
		}
	}
	sink(capture.Block{PCM: pcm, Frames: frames, At: t})
}

// fakeEncoder writes a minimal valid M4A whose audio payload is derived
// from the WAV, and records how many frames each WAV held.
type fakeEncoder struct {
	mu     sync.Mutex
	frames map[string]int64 // output base name → frames
	fail   bool
}

func (e *fakeEncoder) Encode(_ context.Context, wavPath, m4aPath string) error {
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return err
	}
	e.mu.Lock()
	fail := e.fail
	e.mu.Unlock()
	if fail {
		return errTestEncode
	}
	sum := sha256.Sum256(data)
	out := mp4Box("ftyp", []byte("M4A \x00\x00\x02\x00M4A mp42isom"))
	out = append(out, mp4Box("moov", mp4Box("mvhd", make([]byte, 100)))...)
	out = append(out, mp4Box("mdat", sum[:])...)
	if err := os.WriteFile(m4aPath, out, 0o600); err != nil {
		return err
	}
	e.mu.Lock()
	e.frames[filepath.Base(m4aPath)] = int64(len(data)-wav.HeaderSize) / 4
	e.mu.Unlock()
	return nil
}

// lastFrames is the frame count of the most recent encode.
func (e *fakeEncoder) totalFrames() []int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]int64, 0, len(e.frames))
	for _, n := range e.frames {
		out = append(out, n)
	}
	return out
}

func mp4Box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

// sim is one simulated session: clock, Spotify, capture and library.
type sim struct {
	t       *testing.T
	mu      sync.Mutex
	clock   time.Time
	spotify *fakeSpotify
	source  *fakeSource
	enc     *fakeEncoder
	lib     *library.Library
	root    string
	home    string
	events  []Event
	// at runs scripted user actions keyed by simulated time offset.
	at    map[time.Duration]func(*sim)
	start time.Time
}

func newSim(t *testing.T, tracks ...simTrack) *sim {
	t.Helper()
	s := &sim{t: t, clock: time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC), at: map[time.Duration]func(*sim){}}
	s.start = s.clock
	s.spotify = newFakeSpotify(s.now, tracks...)
	s.source = newFakeSource()
	s.enc = &fakeEncoder{frames: map[string]int64{}}
	s.root, s.home = t.TempDir(), t.TempDir()
	s.reopen()
	t.Cleanup(func() {
		if t.Failed() {
			s.mu.Lock()
			defer s.mu.Unlock()
			for _, ev := range s.events {
				b, _ := json.Marshal(ev)
				t.Logf("event %s", b)
			}
			t.Logf("commands %v", s.spotify.trace)
		}
	})
	return s
}

// reopen opens the library afresh, as a new mixtape process would.
func (s *sim) reopen() {
	lib, err := library.Open(s.root, library.Options{
		Encoder: s.enc, WorkDir: filepath.Join(s.home, "work"), Now: s.now,
	})
	require.NoError(s.t, err)
	s.lib = lib
}

func (s *sim) now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clock
}

// run plans cols and executes the session, driving the simulation until
// the engine returns. controls may be nil.
func (s *sim) run(cols []spotify.Collection, o Options, controls chan Control) (*Plan, Result) {
	s.t.Helper()
	s.spotify.mu.Lock()
	s.spotify.watch, s.spotify.wdone = nil, nil // a new process: no watcher yet
	s.spotify.mu.Unlock()
	plan := BuildPlan(s.lib, cols, PlanOptions{Redo: o.Redo})
	require.NoError(s.t, Prepare(s.lib, plan))
	o.Poll = pollDur
	if o.IdleTimeout == 0 {
		o.IdleTimeout = 2 * time.Minute
	}
	eng, err := New(Config{
		Player: s.spotify, Source: s.source, Library: s.lib,
		SpoolDir: filepath.Join(s.home, "spool"), WorkDir: filepath.Join(s.home, "work"),
		Options: o, Now: s.now,
		Emit: func(ev Event) {
			s.mu.Lock()
			s.events = append(s.events, ev)
			s.mu.Unlock()
		},
	})
	require.NoError(s.t, err)

	ctx, cancel := context.WithTimeout(s.t.Context(), 30*time.Second)
	defer cancel()
	done := make(chan struct{})
	go s.drive(ctx, done, eng)
	res := eng.Run(ctx, plan, controls)
	close(done)
	return plan, res
}

// drive advances simulated time in small steps: playback moves, audio is
// captured, and a snapshot is delivered every poll interval.
func (s *sim) drive(ctx context.Context, done <-chan struct{}, eng *Engine) {
	var sincePoll time.Duration
	for {
		select {
		case <-done:
			return
		case <-ctx.Done():
			return
		default:
		}
		s.spotify.mu.Lock()
		watching := s.spotify.watch != nil
		s.spotify.mu.Unlock()
		if !watching || eng.working.Load() > 0 {
			// The engine is setting up, or the library is busy (real disk
			// work): simulated time stands still meanwhile.
			time.Sleep(200 * time.Microsecond)
			continue
		}
		s.mu.Lock()
		s.clock = s.clock.Add(stepDur)
		now := s.clock
		action := s.at[now.Sub(s.start)]
		s.mu.Unlock()
		if action != nil {
			action(s)
		}
		s.spotify.mu.Lock()
		_, ok := s.spotify.current()
		loud := ok && s.spotify.playing
		s.spotify.mu.Unlock()
		s.spotify.advance(stepDur)
		s.source.emit(int(stepDur.Seconds()*testRate), now.Add(-stepDur), loud)

		sincePoll += stepDur
		if sincePoll < pollDur {
			continue
		}
		sincePoll = 0
		s.spotify.mu.Lock()
		ch, wdone := s.spotify.watch, s.spotify.wdone
		s.spotify.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- s.spotify.snapshot():
		case <-wdone:
		case <-done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// types returns the event types emitted, in order.
func (s *sim) types() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.events))
	for _, e := range s.events {
		out = append(out, e.Type)
	}
	return out
}

func (s *sim) eventsOf(typ string) []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Event
	for _, e := range s.events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

func (s *sim) resetEvents() {
	s.mu.Lock()
	s.events = nil
	s.mu.Unlock()
}

// singles builds a resolved track-link collection.
func singles(tracks ...simTrack) []spotify.Collection {
	out := make([]spotify.Collection, 0, len(tracks))
	for _, t := range tracks {
		out = append(out, spotify.Collection{
			Ref: spotify.Ref{Kind: spotify.KindTrack, ID: t.id}, Name: t.name, Source: spotify.SourceEmbed, Complete: true,
			Tracks: []spotify.CatalogTrack{{ID: t.id, Title: t.name, Artist: t.artist, Duration: t.dur, Playable: true, Position: 1}},
		})
	}
	return out
}

// playlist builds a resolved playlist and registers it as a context.
func (s *sim) playlist(id, name string, complete bool, tracks ...simTrack) spotify.Collection {
	col := spotify.Collection{
		Ref: spotify.Ref{Kind: spotify.KindPlaylist, ID: id}, Name: name, Owner: "tester",
		Source: spotify.SourceEmbed, Complete: complete, Truncated: !complete,
	}
	ids := make([]string, 0, len(tracks))
	for i, t := range tracks {
		ids = append(ids, t.id)
		col.Tracks = append(col.Tracks, spotify.CatalogTrack{
			ID: t.id, Title: t.name, Artist: t.artist, Duration: t.dur, Playable: true, Position: i + 1,
		})
	}
	s.spotify.contexts[col.Ref.URI()] = ids
	return col
}

var errTestEncode = errors.New("encoder exploded")
