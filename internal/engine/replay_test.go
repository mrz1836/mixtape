package engine

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// fixtureLine is one record of a golden trace (testdata/*.ndjson), made by
// `mixtape debug trace` against the real Spotify app and trimmed: player
// snapshots, plus the captured audio level every 100 ms.
type fixtureLine struct {
	K     string  `json:"k"` // meta, s (snapshot), a (audio)
	T     int64   `json:"t"` // ns since the trace began
	Rate  int     `json:"rate,omitempty"`
	State string  `json:"st,omitempty"`
	Pos   float64 `json:"p,omitempty"`
	URI   string  `json:"id,omitempty"`
	Name  string  `json:"n,omitempty"`
	DurMS int64   `json:"d,omitempty"`
	Peak  int     `json:"pk,omitempty"`
	N     int     `json:"f,omitempty"` // audio frames in this slice
}

func loadFixture(t *testing.T, name string) (int, []fixtureLine) {
	t.Helper()
	f, err := os.Open(filepath.Join("testdata", name))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var lines []fixtureLine
	rate := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l fixtureLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l))
		if l.K == "meta" {
			rate = l.Rate
			continue
		}
		lines = append(lines, l)
	}
	require.Positive(t, rate)
	return rate, lines
}

// replayPlayer serves a trace's snapshots; commands are recorded and
// otherwise ignored, because the trace is what happened.
type replayPlayer struct {
	mu    sync.Mutex
	ch    chan spotify.Snapshot
	first spotify.Snapshot
	cmds  []string
}

func (p *replayPlayer) log(c string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cmds = append(p.cmds, c)
	return nil
}

func (p *replayPlayer) Poll(context.Context) spotify.Snapshot { return p.first }

func (p *replayPlayer) Watch(context.Context, time.Duration) <-chan spotify.Snapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ch
}

func (p *replayPlayer) PlayTrack(context.Context, string, string) error { return p.log("play") }
func (p *replayPlayer) Play(context.Context) error                      { return p.log("play") }
func (p *replayPlayer) Pause(context.Context) error                     { return p.log("pause") }
func (p *replayPlayer) Next(context.Context) error                      { return p.log("next") }
func (p *replayPlayer) PlayPause(context.Context) error                 { return p.log("playpause") }
func (p *replayPlayer) SetPosition(context.Context, float64) error      { return p.log("seek") }
func (p *replayPlayer) SetShuffle(context.Context, bool) error          { return p.log("shuffle") }
func (p *replayPlayer) SetRepeat(context.Context, bool) error           { return p.log("repeat") }
func (p *replayPlayer) SetVolume(context.Context, int) error            { return p.log("volume") }

// replay runs the engine in live mode over a golden trace and returns the
// events, the encoder that saw every cut, and the trace's sample rate.
func replay(t *testing.T, name string) ([]Event, *fakeEncoder, int) {
	t.Helper()
	rate, lines := loadFixture(t, name)
	t0 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var (
		mu  sync.Mutex
		now = t0
	)
	clock := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	snap := func(l fixtureLine) spotify.Snapshot {
		s := spotify.Snapshot{At: t0.Add(time.Duration(l.T)), State: spotify.State(l.State), Position: l.Pos, Volume: 100}
		s.Track = spotify.Track{URI: l.URI, Name: l.Name, Artist: "Artist", Duration: time.Duration(l.DurMS) * time.Millisecond}
		if id, ok := strings.CutPrefix(l.URI, "spotify:track:"); ok {
			s.Track.ID = id
		}
		return s
	}
	p := &replayPlayer{ch: make(chan spotify.Snapshot)}
	for _, l := range lines {
		if l.K == "s" {
			p.first = snap(l)
			break
		}
	}
	src := &fakeSource{rate: rate, events: make(chan capture.Event)}
	enc := newFakeEncoder()
	home := t.TempDir()
	lib, err := library.Open(t.TempDir(), library.Options{Encoder: enc, WorkDir: filepath.Join(home, "work"), Now: clock})
	require.NoError(t, err)

	var events []Event
	eng, err := New(Config{
		Player: p, Source: src, Library: lib, SpoolDir: filepath.Join(home, "spool"), WorkDir: filepath.Join(home, "work"),
		Options: Options{Poll: 100 * time.Millisecond, IdleTimeout: time.Hour}, Now: clock,
		Emit: func(ev Event) { events = append(events, ev) },
	})
	require.NoError(t, err)
	plan := BuildPlan(lib, nil, PlanOptions{})
	require.NoError(t, Prepare(lib, plan))

	controls := make(chan Control, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	// send delivers a snapshot unless the session is over.
	send := func(s spotify.Snapshot) bool {
		select {
		case p.ch <- s:
			return true
		case <-ctx.Done():
			return false
		}
	}
	var feeder sync.WaitGroup
	feeder.Go(func() {
		for !src.started() {
			select {
			case <-ctx.Done():
				return // capture never started: Run reports why
			case <-time.After(time.Millisecond):
			}
		}
		for _, l := range lines {
			for eng.working.Load() > 0 && ctx.Err() == nil {
				time.Sleep(200 * time.Microsecond)
			}
			mu.Lock()
			now = t0.Add(time.Duration(l.T))
			mu.Unlock()
			switch l.K {
			case "a":
				src.emitLevel(l.N, l.Peak, t0.Add(time.Duration(l.T)))
			case "s":
				if !send(snap(l)) {
					return
				}
			}
		}
		controls <- ControlStop
		// Keep the clock moving so the last song's trailing audio arrives.
		for range 50 {
			mu.Lock()
			now = now.Add(100 * time.Millisecond)
			at := now
			mu.Unlock()
			src.emitLevel(rate/10, 0, at)
			if !send(spotify.Snapshot{At: at, State: spotify.StatePaused}) {
				return
			}
		}
	})
	eng.Run(ctx, plan, controls)
	cancel() // the feeder may still be offering snapshots nobody reads
	feeder.Wait()
	return events, enc, rate
}

// TestGoldenTraceCompleteSongWithPause replays a real Spotify session:
// "Speak to Me" (1:04) played from the top, paused for 4 s at 0:20,
// resumed, played out gaplessly into "Breathe", which was paused 7 s in.
func TestGoldenTraceCompleteSongWithPause(t *testing.T) {
	t.Parallel()
	events, enc, rate := replay(t, "trace-complete.ndjson")

	var saved, discarded []Event
	for _, ev := range events {
		switch ev.Type {
		case EventTrackSaved:
			saved = append(saved, ev)
		case EventTrackDiscarded:
			discarded = append(discarded, ev)
		}
	}
	require.Len(t, saved, 1, "events: %+v", events)
	assert.Equal(t, "Speak to Me", saved[0].Track.Title)
	require.Len(t, discarded, 1)
	assert.Equal(t, "Breathe (In the Air)", discarded[0].Track.Title)
	assert.Equal(t, ReasonEndedEarly, discarded[0].Reason)

	// The saved file is the song's length (the 4 s pause excised) plus the
	// margins, to within 50 ms.
	frames := enc.totalFrames()
	require.Len(t, frames, 1)
	want := float64(64333)/1000 + 2*DefaultTuning().Margin.Seconds()
	assert.InDelta(t, want, float64(frames[0])/float64(rate), 0.05)
}
