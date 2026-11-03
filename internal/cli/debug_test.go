package cli

import (
	"bufio"
	"context"
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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/capture/device"
	"github.com/mrz1836/mixtape/internal/capture/tap"
	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/spotify"
)

var errOsascriptTimeout = errors.New("osascript: timed out")

// traceBlockFrames is one 100 ms block at the fake sources' 8 kHz.
const traceBlockFrames = 800

// pcmBlock returns frames of stereo PCM at bits bits a sample whose every
// sample is level, in 16-bit units: at 24 bits with a busy low byte below.
func pcmBlock(frames, level, bits int) []byte {
	if bits != 24 {
		pcm := make([]byte, frames*4)
		for i := 0; i < len(pcm); i += 2 {
			binary.LittleEndian.PutUint16(pcm[i:], uint16(level))
		}
		return pcm
	}
	pcm := make([]byte, 0, frames*6)
	v := level<<8 | 0x33
	for range frames * 2 {
		pcm = append(pcm, byte(v), byte(v>>8), byte(v>>16))
	}
	return pcm
}

// traceSource is a capture backend that, once started, delivers one
// 100 ms block per entry of loud (audible or silent), stamped from the
// moment it started, as a backend's callbacks would be.
type traceSource struct {
	nopSource

	bits     int // as Format reports it
	loud     []bool
	startErr error
	mu       sync.Mutex
	started  time.Time
}

func (s *traceSource) Format() capture.Format {
	return capture.Format{Rate: 8000, Channels: 2, Bits: s.bits}
}

func (s *traceSource) Start(_ context.Context, sink func(capture.Block)) error {
	if s.startErr != nil {
		return s.startErr
	}
	s.mu.Lock()
	s.started = time.Now()
	start := s.started
	s.mu.Unlock()
	for i, loud := range s.loud {
		level := 0
		if loud {
			level = 4000
		}
		sink(capture.Block{
			PCM: pcmBlock(traceBlockFrames, level, s.bits), Frames: traceBlockFrames,
			At: start.Add(time.Duration(i) * 100 * time.Millisecond),
		})
	}
	return nil
}

func (s *traceSource) startedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// timedSnap is a player observation at an offset from the capture start.
type timedSnap struct {
	at   time.Duration
	snap spotify.Snapshot
}

// tracePlayer is Spotify as a trace watches it: the scripted observations,
// on the capture's clock, and then the end of the watch.
type tracePlayer struct {
	*fakePlayer

	src   *traceSource
	snaps []timedSnap
}

func (p *tracePlayer) Watch(ctx context.Context, _ time.Duration) <-chan spotify.Snapshot {
	start := p.src.startedAt()
	ch := make(chan spotify.Snapshot)
	go func() {
		defer close(ch)
		for _, s := range p.snaps {
			s.snap.At = start.Add(s.at)
			select {
			case ch <- s.snap:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}

func playingSnap(id, name string, pos float64) spotify.Snapshot {
	return spotify.Snapshot{
		State: spotify.StatePlaying, Position: pos, Volume: 100,
		Track: spotify.Track{URI: "spotify:track:" + id, Name: name, Duration: 200 * time.Second},
	}
}

// traceScript is a short session: a failed observation, a paused song,
// then a track change whose first position is still the old song's.
// Audio is silent for 0.6 s, then loud; the new song's positions put its
// start 50 ms after the audio onset.
func traceScript() (*traceSource, []timedSnap) {
	loud := make([]bool, 30)
	for i := range loud {
		loud[i] = i >= 6
	}
	at := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	paused := playingSnap(trkA, "Intro", 30)
	paused.State = spotify.StatePaused
	return &traceSource{loud: loud}, []timedSnap{
		{at(0), spotify.Snapshot{Err: errOsascriptTimeout}},
		{at(50), paused},
		{at(1000), playingSnap(trkB, "Queen Anne's Lace", 30.1)},
		{at(1200), playingSnap(trkB, "Queen Anne's Lace", 0.55)},
		{at(1400), playingSnap(trkB, "Queen Anne's Lace", 0.75)},
		{at(1600), playingSnap(trkB, "Queen Anne's Lace", 0.95)},
	}
}

// traceHarness wires a scripted trace into a harness.
func traceHarness(t *testing.T) (*harness, *traceSource) {
	t.Helper()
	h := newHarness(t)
	src, snaps := traceScript()
	h.deps.services.openCapture = func(_ context.Context, r captureRequest) (capture.Source, error) {
		assert.Equal(t, capture.MethodTap, r.Method, "the tap is this Mac's default method")
		src.bits = r.Bits
		return src, nil
	}
	h.deps.services.player = func() player { return &tracePlayer{fakePlayer: h.world.player, src: src, snaps: snaps} }
	return h, src
}

// readTraceFile decodes every line of a trace file.
func readTraceFile(t *testing.T, path string) []traceLine {
	t.Helper()
	f, err := os.Open(filepath.Clean(path))
	require.NoError(t, err)
	defer func() { _ = f.Close() }()
	var lines []traceLine
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l traceLine
		require.NoError(t, json.Unmarshal(sc.Bytes(), &l), sc.Text())
		lines = append(lines, l)
	}
	require.NoError(t, sc.Err())
	return lines
}

// writeTraceFile writes lines as a trace file and returns its path.
func writeTraceFile(t *testing.T, lines ...traceLine) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.ndjson")
	var b strings.Builder
	for _, l := range lines {
		data, err := json.Marshal(l)
		require.NoError(t, err)
		b.Write(data)
		b.WriteByte('\n')
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
	return path
}

// A trace finds the same onsets at any capture depth: its peaks are on the
// 16-bit scale whatever the samples hold.
func TestDebugTraceRecordsSnapshotsAndAudioOnOneClock(t *testing.T) {
	t.Parallel()
	for name, depth := range map[string]string{"24-bit by default": "", "16-bit": "16"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			debugTraceOnOneClock(t, depth)
		})
	}
}

func debugTraceOnOneClock(t *testing.T, depth string) {
	h, src := traceHarness(t)
	bits := 24
	if depth != "" {
		h.env[config.EnvBitDepth] = depth
		bits = 16
	}
	out := filepath.Join(t.TempDir(), "traces", "run.ndjson")

	require.Equal(t, ExitOK, h.run("debug", "trace", "--out", out, "--poll", "50ms", "-o", "text"), h.stderr.String())
	require.Equal(t, bits, src.bits, "the trace captures at the configured depth")
	text := h.stdout.String()
	assert.Contains(t, text, fmt.Sprintf("tracing for 1m0s with fake device at %d-bit 8 kHz into %s", bits, out))
	assert.Contains(t, text, "trace "+out+": 6 snapshots, 30 audio blocks at 8000 Hz")
	assert.Regexp(t, `1\.00s change Queen Anne's Lace\s+stale=1  onset−estimate = -0\.050s`, text,
		"the stale position is set aside and the onset leads the positions by 50 ms")
	assert.Contains(t, text, "position step: min 0.200s, median 0.200s, max 0.200s")
	assert.Positive(t, src.closes.Load(), "the capture is closed before the trace is flushed")

	lines := readTraceFile(t, out)
	require.Len(t, lines, 1+30+6)
	assert.Equal(t, traceLine{K: "meta", Rate: 8000, Chans: 2, Bits: bits, Src: "fake device", PollM: 50}, lines[0])
	for i, l := range lines[1:31] {
		assert.Equal(t, "audio", l.K)
		assert.Equal(t, int64(i*traceBlockFrames), l.Frame, "frames count on from the first block")
		assert.Equal(t, traceBlockFrames, l.N)
		if i < 6 {
			assert.Zero(t, l.Peak, "block %d is silent", i)
		} else {
			assert.Equal(t, 4000, l.Peak, "block %d is loud", i)
		}
	}
	snaps := lines[31:]
	assert.Equal(t, "snap", snaps[0].K)
	assert.Equal(t, errOsascriptTimeout.Error(), snaps[0].Err, "failed observations are kept")
	change := snaps[2]
	assert.Equal(t, "playing", change.State)
	assert.InDelta(t, 30.1, change.Pos, 1e-9)
	assert.Equal(t, "spotify:track:"+trkB, change.ID)
	assert.Equal(t, "Queen Anne's Lace", change.Name)
	assert.Equal(t, int64(200000), change.DurMS)
	assert.Equal(t, 100, change.Vol)
	assert.Equal(t, time.Second, time.Duration(change.T-lines[1].T), "snapshots and audio share one clock")
}

func TestDebugTraceSavesUnderTheHomeByDefault(t *testing.T) {
	t.Parallel()
	h, _ := traceHarness(t)

	require.Equal(t, ExitOK, h.run("debug", "trace"), h.stderr.String())
	want := filepath.Join(h.env["MIXTAPE_HOME"], "traces", "20261101-120000.ndjson")
	assert.Equal(t, map[string]any{"path": want}, h.json(), "JSON mode prints only where the trace is")
	assert.Len(t, readTraceFile(t, want), 37)
}

func TestCtrlCEndsATraceEarlyAndStillAnalyzesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.services.signals = stopAtOnce
	src := &traceSource{loud: []bool{false, true}}
	h.deps.services.openCapture = func(context.Context, captureRequest) (capture.Source, error) { return src, nil }
	out := filepath.Join(t.TempDir(), "early.ndjson")

	require.Equal(t, ExitOK, h.run("debug", "trace", "--duration", "10m", "--out", out, "-o", "text"))
	assert.Contains(t, h.stdout.String(), ": 0 snapshots, 2 audio blocks at 8000 Hz")
	assert.NotContains(t, h.stdout.String(), "position step", "no playback, no step")
}

func TestDebugTraceFailures(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	tests := map[string]struct {
		setup  func(h *harness, src *traceSource)
		args   []string
		exit   int
		hint   string
		noFile bool
	}{
		"an unknown capture method": {
			args: []string{"--capture", "magic"}, exit: ExitUsage, noFile: true,
		},
		"the tap on a Mac without it": {
			setup: func(h *harness, _ *traceSource) { h.world.tap = capture.TapStatus{Reason: "macOS 13.6"} },
			args:  []string{"--capture", "tap"}, exit: ExitNotReady, hint: "--capture device", noFile: true,
		},
		"no capture device": {
			setup: func(h *harness, _ *traceSource) {
				h.deps.services.openCapture = func(context.Context, captureRequest) (capture.Source, error) {
					return nil, fmt.Errorf("open BlackHole: %w", device.ErrNoDevice)
				}
			},
			args: []string{"--capture", "device"}, exit: ExitNotReady, hint: "brew install blackhole-2ch", noFile: true,
		},
		"a trace folder that is a file": {
			args: []string{"--out", filepath.Join(blocker, "t.ndjson")}, exit: ExitError,
		},
		"a tap macOS refuses": {
			setup: func(_ *harness, src *traceSource) { src.startErr = tap.ErrCreateTap },
			exit:  ExitPermission, hint: "System Audio Recording",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h, src := traceHarness(t)
			if tc.setup != nil {
				tc.setup(h, src)
			}
			out := filepath.Join(t.TempDir(), "t.ndjson")
			args := append([]string{"debug", "trace", "--out", out}, tc.args...)
			require.Equal(t, tc.exit, h.run(args...), h.stderr.String())
			e := h.errDoc()
			assert.Equal(t, codeFor(tc.exit), e["code"])
			if tc.hint != "" {
				assert.Contains(t, e["hint"], tc.hint)
			}
			if tc.noFile {
				assert.NoFileExists(t, out, "nothing is written before the capture opens")
			}
		})
	}
}

func TestTraceAnalysisSeparatesTrackChangesFromResumes(t *testing.T) {
	t.Parallel()
	// 1 kHz, 100-frame blocks every 100 ms: silent until 0.6 s, loud until
	// 2.0 s (the pause), silent again until 2.4 s, then loud.
	lines := []traceLine{{K: "meta", Rate: 1000, Chans: 2}}
	for i := range 50 {
		peak := 0
		if (i >= 6 && i < 20) || i >= 24 {
			peak = 3000
		}
		lines = append(lines, traceLine{K: "audio", T: int64(i) * 1e8, Frame: int64(i) * 100, N: 100, Peak: peak})
	}
	snap := func(ms int64, state, id, name string, pos float64) traceLine {
		return traceLine{K: "snap", T: ms * 1e6, State: state, ID: id, Name: name, Pos: pos}
	}
	lines = append(lines,
		snap(50, "paused", "A", "Intro", 30),
		snap(1000, "playing", "B", "Second", 0.35),
		snap(1200, "playing", "B", "Second", 0.55),
		snap(2000, "paused", "B", "Second", 1.30),
		snap(2500, "playing", "B", "Second", 1.35),
		snap(2700, "playing", "B", "Second", 1.55),
	)
	path := writeTraceFile(t, lines...)

	got := analyzeTrace(path)
	assert.Contains(t, got, "trace "+path+": 6 snapshots, 50 audio blocks at 1000 Hz")
	assert.Regexp(t, `1\.00s change Second\s+stale=0  onset−estimate = -0\.050s`, got)
	assert.Regexp(t, `2\.50s resume Second\s+stale=0  onset−estimate = -0\.050s`, got,
		"a resume is measured from the paused position, so it shows the same latency as a change")
	assert.Equal(t, 3, strings.Count(got, "\n  "), "two starts and the position step")
	assert.Contains(t, got, "position step: min 0.200s, median 0.200s, max 0.200s")
}

func TestTraceAnalysisSetsStalePositionsAside(t *testing.T) {
	t.Parallel()
	loudAudio := func(n int) []traceLine {
		out := make([]traceLine, 0, 1+n)
		out = append(out, traceLine{K: "meta", Rate: 1000})
		for i := range n {
			out = append(out, traceLine{K: "audio", T: int64(i) * 1e8, Frame: int64(i) * 100, N: 100, Peak: 3000})
		}
		return out
	}
	snap := func(ms int64, id string, pos float64) traceLine {
		return traceLine{K: "snap", T: ms * 1e6, State: "playing", ID: id, Name: "Song " + id, Pos: pos}
	}

	t.Run("the old song's positions under the new one are counted", func(t *testing.T) {
		t.Parallel()
		path := writeTraceFile(t, append(loudAudio(30),
			snap(0, "A", 10), snap(1000, "B", 11.1), snap(1100, "B", 11.2), snap(1300, "B", 0.2))...)
		got := analyzeTrace(path)
		assert.Regexp(t, `1\.00s change Song B\s+stale=2`, got)
		assert.NotContains(t, got, "onset", "audio that never went quiet has no onset")
	})

	t.Run("a change with only stale positions is left out", func(t *testing.T) {
		t.Parallel()
		path := writeTraceFile(t, append(loudAudio(30),
			snap(0, "A", 10), snap(1000, "B", 11.1), snap(1100, "B", 11.2))...)
		got := analyzeTrace(path)
		assert.NotRegexp(t, `\ds change`, got)
		assert.Contains(t, got, "3 snapshots, 30 audio blocks")
	})
}

func TestTraceWithoutMetadataSaysSo(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "none.ndjson")
	assert.Equal(t, "trace "+missing+": no metadata", analyzeTrace(missing))

	path := filepath.Join(t.TempDir(), "partial.ndjson")
	require.NoError(t, os.WriteFile(path, []byte("garbage\n{\"k\":\"snap\",\"t\":1}\n"), 0o600))
	assert.Equal(t, "trace "+path+": no metadata", analyzeTrace(path))
}

func TestReadTraceSkipsBrokenLines(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "t.ndjson")
	data := "{\"k\":\"meta\",\"rate\":48000}\nnot json\n{\"k\":\"snap\",\"t\":5,\"pos\":1.5}\n"
	require.NoError(t, os.WriteFile(path, []byte(data), 0o600))

	lines, rate := readTrace(path)
	assert.Equal(t, 48000, rate)
	require.Len(t, lines, 2)
	assert.Equal(t, traceLine{K: "snap", T: 5, Pos: 1.5}, lines[1])

	lines, rate = readTrace(filepath.Join(t.TempDir(), "missing"))
	assert.Nil(t, lines)
	assert.Zero(t, rate)
}

func TestInterpolateFrameFollowsTheBlockTimestamps(t *testing.T) {
	t.Parallel()
	assert.Zero(t, interpolateFrame(nil, 5e8, 1000), "no audio, no frame")
	audio := []traceLine{{T: 1e9, Frame: 0}, {T: 1.1e9, Frame: 100}, {T: 1.2e9, Frame: 200}}
	tests := map[string]struct {
		at   int64
		want float64
	}{
		"between two blocks":   {1.05e9, 50},
		"at a block":           {1.1e9, 100},
		"after the last block": {1.25e9, 250},
		"before the first":     {0.5e9, -500},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.InDelta(t, tc.want, interpolateFrame(audio, tc.at, 1000), 1e-9)
		})
	}
}

func TestAudioOnsetNeedsSilenceFirst(t *testing.T) {
	t.Parallel()
	// blocks lays out 100 ms blocks of the given peaks from time 0.
	blocks := func(peaks ...int) []traceLine {
		out := make([]traceLine, 0, len(peaks))
		for i, p := range peaks {
			out = append(out, traceLine{T: int64(i) * 1e8, Frame: int64(i) * 100, Peak: p})
		}
		return out
	}
	tests := map[string]struct {
		audio []traceLine
		from  int64
		want  float64
	}{
		"loud after 300 ms of silence":                {blocks(0, 0, 0, 500), 0, 300},
		"near-silent samples count as silence":        {blocks(16, 16, 16, 300), 0, 300},
		"100 ms of silence is a gap, not a start":     {blocks(500, 0, 500, 500), 0, -1},
		"a fade-in starts at its first audible block": {blocks(0, 0, 0, 100, 500), 0, 300},
		"sound that falls quiet again was no start":   {blocks(0, 0, 0, 100, 0, 0, 0, 500), 0, 700},
		"too little silence before a fade-in":         {blocks(500, 0, 100, 500), 0, -1},
		"a fade-in may dip back to quiet":             {blocks(0, 0, 0, 100, 0, 100, 500), 0, 300},
		"the silence may begin before from":           {blocks(0, 0, 0, 500, 500), 3e8, 300},
		"a rise before from is not this start's":      {blocks(0, 0, 500, 500, 500), 3e8, -1},
		"never loud":                                  {blocks(0, 0, 0, 0), 0, -1},
		"an onset past 5 s is not this start's":       {blocks(append(make([]int, 55), 500)...), 0, -1},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			assert.InDelta(t, tc.want, audioOnset(tc.audio, tc.from), 0)
		})
	}
}

func TestStartAnchorUsesTheFirstTwoSecondsOfOnePlayback(t *testing.T) {
	t.Parallel()
	audio := []traceLine{{T: 0, Frame: 0}}
	snap := func(ms int64, state, id string, pos float64) traceLine {
		return traceLine{T: ms * 1e6, State: state, ID: id, Pos: pos}
	}

	// A resume: the song keeps its position, so nothing is stale.
	resume := []traceLine{snap(0, "paused", "A", 60), snap(1000, "playing", "A", 60), snap(1500, "playing", "A", 60.5)}
	anchor, stale, ok := startAnchor(resume, 1, audio, 1000)
	require.True(t, ok)
	assert.Zero(t, stale)
	assert.InDelta(t, -59000, anchor, 1e-6, "position 0 was 59 s before the trace began")

	// The window ends at a pause, at another track, or after 2 s.
	for name, next := range map[string]traceLine{
		"pause":         snap(1500, "paused", "B", 0.5),
		"another track": snap(1500, "playing", "C", 0.5),
		"two seconds":   snap(3000, "playing", "B", 999),
	} {
		snaps := []traceLine{snap(0, "playing", "A", 10), snap(1000, "playing", "B", 0.1), next}
		got, gotStale, gotOK := startAnchor(snaps, 1, audio, 1000)
		require.True(t, gotOK, name)
		assert.Zero(t, gotStale, name)
		assert.InDelta(t, 900, got, 1e-6, "%s: only the first snapshot counts", name)
	}

	// Nothing but stale positions: no anchor.
	_, stale, ok = startAnchor([]traceLine{snap(0, "playing", "A", 10), snap(1000, "playing", "B", 9)}, 1, audio, 1000)
	assert.False(t, ok)
	assert.Equal(t, 1, stale)
}
