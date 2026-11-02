package cli

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// fakePlayer answers polls with the scripted snapshots in polls, then with
// snap, and records commands.
type fakePlayer struct {
	mu       sync.Mutex
	snap     spotify.Snapshot
	polls    []spotify.Snapshot
	commands []string
}

func (p *fakePlayer) cmd(c string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands = append(p.commands, c)
	return nil
}

func (p *fakePlayer) Commands() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.commands...)
}

func (p *fakePlayer) Poll(context.Context) spotify.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.polls) > 0 {
		s := p.polls[0]
		p.polls = p.polls[1:]
		return s
	}
	return p.snap
}

func (p *fakePlayer) Watch(ctx context.Context, _ time.Duration) <-chan spotify.Snapshot {
	ch := make(chan spotify.Snapshot)
	go func() {
		<-ctx.Done()
		close(ch)
	}()
	return ch
}

func (p *fakePlayer) PlayTrack(context.Context, string, string) error { return p.cmd("play") }
func (p *fakePlayer) Play(context.Context) error                      { return p.cmd("play") }
func (p *fakePlayer) Pause(context.Context) error                     { return p.cmd("pause") }
func (p *fakePlayer) Next(context.Context) error                      { return p.cmd("next") }
func (p *fakePlayer) PlayPause(context.Context) error                 { return p.cmd("playpause") }
func (p *fakePlayer) SetPosition(context.Context, float64) error      { return p.cmd("seek") }
func (p *fakePlayer) SetShuffle(context.Context, bool) error          { return p.cmd("shuffle") }
func (p *fakePlayer) SetRepeat(context.Context, bool) error           { return p.cmd("repeat") }
func (p *fakePlayer) SetVolume(context.Context, int) error            { return p.cmd("volume") }

// fakeCatalog resolves refs from a map.
type fakeCatalog struct {
	cols  map[spotify.Ref]spotify.Collection
	err   error
	dates map[string]string // track ID → release date
}

func (c *fakeCatalog) ReleaseDate(_ context.Context, id string) (string, error) {
	return c.dates[id], nil
}

func (c *fakeCatalog) Resolve(_ context.Context, ref spotify.Ref) (spotify.Collection, error) {
	if c.err != nil {
		return spotify.Collection{}, c.err
	}
	col, ok := c.cols[ref]
	if !ok {
		return spotify.Collection{}, spotify.ErrNotFound
	}
	return col, nil
}

// nopSource is a capture source that never delivers audio. It counts
// how often it is closed.
type nopSource struct {
	events chan capture.Event
	closes atomic.Int32
}

func (s *nopSource) Name() string           { return "fake device" }
func (s *nopSource) Method() capture.Method { return capture.MethodDevice }
func (s *nopSource) Format() capture.Format { return capture.Format{Rate: 8000, Channels: 2} }
func (s *nopSource) Start(context.Context, func(capture.Block)) error {
	return nil
}
func (s *nopSource) Events() <-chan capture.Event { return s.events }
func (s *nopSource) Close() error                 { s.closes.Add(1); return nil }

// syntheticEncoder writes a minimal M4A whose audio is a hash of the WAV.
type syntheticEncoder struct{}

func (syntheticEncoder) Encode(_ context.Context, wavPath, m4aPath string) error {
	data, err := os.ReadFile(wavPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	out := box("ftyp", []byte("M4A \x00\x00\x02\x00M4A mp42isom"))
	out = append(out, box("moov", box("mvhd", make([]byte, 100)))...)
	out = append(out, box("mdat", sum[:])...)
	return os.WriteFile(m4aPath, out, 0o600)
}

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload)))
	copy(b[4:], typ)
	return append(b, payload...)
}

// fakeWorld is the simulated machine behind the harness services.
type fakeWorld struct {
	mu         sync.Mutex
	player     *fakePlayer
	catalog    *fakeCatalog
	installed  bool
	running    bool
	runningErr error // the process list could not be read
	devicesErr error
	prefs      spotify.Prefs
	tap        capture.TapStatus
	devices    []device.Info
	output     device.Info
	lockPath   string
	killed     []int
	onKill     func(pid int)
	killErr    error // returned by kill after onKill
	awake      int   // keepAwake holds not yet released
	awakeCalls int
	onSleep    func() // runs after each fake sleep
	// perms answers permission checks (missing kinds are granted); grantAfter
	// flips a kind to granted after that many checks, like a user switching
	// it on in System Settings.
	perms      map[permission.Kind]permission.State
	grantAfter map[permission.Kind]int
	scripts    []string
}

func (w *fakeWorld) permission(k permission.Kind) permission.State {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n, ok := w.grantAfter[k]; ok {
		if n <= 0 {
			w.perms[k] = permission.Granted
		}
		w.grantAfter[k] = n - 1
	}
	if st, ok := w.perms[k]; ok {
		return st
	}
	return permission.Granted
}

func newFakeWorld(t *testing.T) *fakeWorld {
	t.Helper()
	return &fakeWorld{
		player:    &fakePlayer{snap: spotify.Snapshot{State: spotify.StatePaused, Volume: 100}},
		catalog:   &fakeCatalog{cols: map[spotify.Ref]spotify.Collection{}},
		installed: true, running: true,
		prefs: spotify.Prefs{Path: "/prefs"},
		tap:   capture.TapStatus{Supported: true},
		devices: []device.Info{
			{Name: "BlackHole 2ch", Input: true, Output: true},
			{Name: "MacBook Pro Speakers", Output: true, DefaultOutput: true},
		},
		output:     device.Info{Name: "MacBook Pro Speakers", Output: true, DefaultOutput: true},
		lockPath:   filepath.Join(t.TempDir(), "recorder.lock"),
		perms:      map[permission.Kind]permission.State{},
		grantAfter: map[permission.Kind]int{},
	}
}

// services wires the fake world into the CLI.
func (w *fakeWorld) services(h *harness) services {
	return services{
		player:         func() player { return w.player },
		catalog:        func() catalog { return w.catalog },
		openCapture:    func(captureRequest) (capture.Source, error) { return &nopSource{events: make(chan capture.Event)}, nil },
		tapStatus:      func() capture.TapStatus { return w.tap },
		devices:        func() ([]device.Info, error) { return w.devices, w.devicesErr },
		defaultOutput:  func() (device.Info, error) { return w.output, nil },
		spotifyRunning: func() (bool, error) { return w.running, w.runningErr },
		spotifyApp:     func(string) (string, bool) { return "/Applications/Spotify.app", w.installed },
		prefs:          func(string) (spotify.Prefs, error) { return w.prefs, nil },
		lockPath:       func() (string, error) { return w.lockPath, nil },
		executable:     os.Executable,
		lookPath:       func(string) (string, error) { return "/usr/bin/afconvert", nil },
		encoder:        syntheticEncoder{},
		signals:        func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} },
		spawn:          func(string, []string, []string, string) (*os.Process, error) { return nil, os.ErrInvalid },
		keepAwake: func() func() {
			w.mu.Lock()
			w.awake++
			w.awakeCalls++
			w.mu.Unlock()
			return func() {
				w.mu.Lock()
				w.awake--
				w.mu.Unlock()
			}
		},
		kill: func(pid int, _ syscall.Signal) error {
			w.mu.Lock()
			w.killed = append(w.killed, pid)
			fn, err := w.onKill, w.killErr
			w.mu.Unlock()
			if fn != nil {
				fn(pid)
			}
			return err
		},
		sleep: func(ctx context.Context, d time.Duration) bool {
			h.advance(d)
			w.mu.Lock()
			fn := w.onSleep
			w.mu.Unlock()
			if fn != nil {
				fn()
			}
			return ctx.Err() == nil
		},
		runScript: func(_ context.Context, name string, args ...string) error {
			w.mu.Lock()
			defer w.mu.Unlock()
			w.scripts = append(w.scripts, strings.Join(append([]string{name}, args...), " "))
			return nil
		},
		permission:        w.permission,
		requestPermission: w.permission,
		hostApp:           func() permission.HostApp { return permission.HostApp{Name: "iTerm"} },
	}
}

// addPlaylist registers the test playlist (plID) with tracks.
func (w *fakeWorld) addPlaylist(name string, tracks ...spotify.CatalogTrack) spotify.Ref {
	ref := spotify.Ref{Kind: spotify.KindPlaylist, ID: plID}
	for i := range tracks {
		tracks[i].Position = i + 1
		tracks[i].Playable = true
	}
	w.catalog.cols[ref] = spotify.Collection{
		Ref: ref, Name: name, Owner: "tester", Tracks: tracks, Complete: true, Source: spotify.SourceEmbed,
	}
	return ref
}

// recordDone stores a finished recording of t in the collection for spec.
func recordDone(t *testing.T, root string, spec library.Spec, tr library.Track) {
	t.Helper()
	lib, err := library.Open(root, library.Options{Encoder: syntheticEncoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	c, err := lib.Collection(spec)
	require.NoError(t, err)
	wavPath := filepath.Join(t.TempDir(), "in.wav")
	require.NoError(t, os.WriteFile(wavPath, make([]byte, 64), 0o600))
	_, err = lib.Commit(t.Context(), c, tr, wavPath, library.CommitOptions{RecordedMS: tr.DurationMS})
	require.NoError(t, err)
}

// loudSource is a capture source that delivers one audible block on Start.
type loudSource struct{ nopSource }

func (s *loudSource) Start(_ context.Context, sink func(capture.Block)) error {
	pcm := make([]byte, 4*512)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], 4000)
	}
	sink(capture.Block{PCM: pcm, Frames: 512, At: time.Now()})
	return nil
}
