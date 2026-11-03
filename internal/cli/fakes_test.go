package cli

import (
	"context"
	"encoding/binary"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
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

func (p *fakePlayer) PlayTrack(context.Context, string, string) error { return p.cmd("playtrack") }
func (p *fakePlayer) Play(context.Context) error                      { return p.cmd("play") }
func (p *fakePlayer) Pause(context.Context) error                     { return p.cmd("pause") }
func (p *fakePlayer) Next(context.Context) error                      { return p.cmd("next") }
func (p *fakePlayer) PlayPause(context.Context) error                 { return p.cmd("playpause") }
func (p *fakePlayer) SetPosition(context.Context, float64) error      { return p.cmd("seek") }
func (p *fakePlayer) SetShuffle(context.Context, bool) error          { return p.cmd("shuffle") }
func (p *fakePlayer) SetRepeat(context.Context, bool) error           { return p.cmd("repeat") }
func (p *fakePlayer) SetVolume(context.Context, int) error            { return p.cmd("volume") }

// fakeCatalog resolves refs from a map; err, when set, fails every lookup.
type fakeCatalog struct {
	cols map[spotify.Ref]spotify.Collection
	err  error
}

func (c *fakeCatalog) ReleaseDate(context.Context, string) (string, error) {
	return "", c.err
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
	// encoders are the afconvert stand-ins made so far, and encodedAs
	// the encoder each one stood in for.
	encoders  []*testutil.Encoder
	encodedAs []library.AFConvert
	// out is the machine's output devices.
	out *fakeOutput
}

// fakeOutput is a machine's output devices, by UID: the default output,
// each one's rate and the rates it offers, and every rate set.
type fakeOutput struct {
	mu      sync.Mutex
	def     string
	names   map[string]string
	rates   map[string]int
	offers  map[string][]capture.RateRange
	sets    []string
	readErr error
}

func newFakeOutput() *fakeOutput {
	return &fakeOutput{
		def:    "spk",
		names:  map[string]string{"spk": "MacBook Pro Speakers"},
		rates:  map[string]int{"spk": 48000},
		offers: map[string][]capture.RateRange{"spk": {{Min: 44100, Max: 44100}, {Min: 48000, Max: 48000}}},
	}
}

func (f *fakeOutput) DefaultOutput() (capture.OutputDevice, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return capture.OutputDevice{}, f.readErr
	}
	return capture.OutputDevice{UID: f.def, Name: f.names[f.def], Rate: f.rates[f.def], Rates: f.offers[f.def]}, nil
}

func (f *fakeOutput) OutputRate(uid string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.rates[uid]
	if !ok {
		return 0, capture.ErrOutputGone
	}
	return r, nil
}

func (f *fakeOutput) SetOutputRate(_ context.Context, uid string, rate int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, uid+"="+strconv.Itoa(rate))
	if _, ok := f.rates[uid]; !ok {
		return capture.ErrOutputGone
	}
	f.rates[uid] = rate
	return nil
}

// state returns every device's rate and the sets made so far.
func (f *fakeOutput) state() (map[string]int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.rates), slices.Clone(f.sets)
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
		out:        newFakeOutput(),
	}
}

// encoder is the stand-in for afconvert: a testutil.Encoder in the codec
// the settings ask for. Every one made is kept, so tests can see what was
// encoded.
func (w *fakeWorld) encoder(a library.AFConvert) library.Encoder {
	w.mu.Lock()
	defer w.mu.Unlock()
	enc := &testutil.Encoder{Codec: string(a.Codec)}
	w.encoders = append(w.encoders, enc)
	w.encodedAs = append(w.encodedAs, a)
	return enc
}

// services wires the fake world into the CLI.
func (w *fakeWorld) services(h *harness) services {
	return services{
		player:  func() player { return w.player },
		catalog: func() catalog { return w.catalog },
		openCapture: func(context.Context, captureRequest) (capture.Source, error) {
			return &nopSource{events: make(chan capture.Event)}, nil
		},
		tapStatus:      func() capture.TapStatus { return w.tap },
		output:         func() capture.OutputControl { return w.out },
		devices:        func() ([]device.Info, error) { return w.devices, w.devicesErr },
		defaultOutput:  func() (device.Info, error) { return w.output, nil },
		spotifyRunning: func() (bool, error) { return w.running, w.runningErr },
		spotifyApp:     func(string) (string, bool) { return "/Applications/Spotify.app", w.installed },
		prefs:          func(string) (spotify.Prefs, error) { return w.prefs, nil },
		lockPath:       func() (string, error) { return w.lockPath, nil },
		executable:     os.Executable,
		lookPath:       func(string) (string, error) { return "/usr/bin/afconvert", nil },
		encoder:        w.encoder,
		afconvertHelp: func(context.Context) string {
			return "    'm4af' = Apple MPEG-4 Audio (.m4a, .m4r)\n               data_formats: 'aac ' 'alac' \n"
		},
		signals: func() (<-chan os.Signal, func()) { return make(chan os.Signal), func() {} },
		spawn:   func(string, []string, []string, string) (*os.Process, error) { return nil, os.ErrInvalid },
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
