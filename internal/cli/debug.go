package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/config"
)

// traceLine is one record of a timing trace. T is nanoseconds since the
// trace began, on the monotonic clock shared by snapshots and audio.
type traceLine struct {
	K     string  `json:"k"` // meta, snap, audio
	T     int64   `json:"t"`
	State string  `json:"state,omitempty"`
	Pos   float64 `json:"pos,omitempty"`
	ID    string  `json:"id,omitempty"`
	Name  string  `json:"name,omitempty"`
	DurMS int64   `json:"dur_ms,omitempty"`
	Vol   int     `json:"vol,omitempty"`
	Err   string  `json:"err,omitempty"`
	Frame int64   `json:"frame,omitempty"` // audio: index of the block's first frame
	N     int     `json:"n,omitempty"`     // audio: frames in the block
	Peak  int     `json:"peak,omitempty"`  // audio: loudest sample
	Rate  int     `json:"rate,omitempty"`  // meta
	Chans int     `json:"chans,omitempty"` // meta
	Src   string  `json:"src,omitempty"`   // meta
	PollM int64   `json:"poll_ms,omitempty"`
}

func newDebugCmd(a *app) *cobra.Command {
	cmd := &cobra.Command{
		Use:    "debug",
		Short:  "Diagnostics for tuning and bug reports",
		Long:   `Diagnostics for tuning mixtape's timing and for bug reports. Not part of the stable interface.`,
		Hidden: true,
		Args:   cobra.NoArgs,
	}
	cmd.AddCommand(newTraceCmd(a))
	return cmd
}

func newTraceCmd(a *app) *cobra.Command {
	var (
		duration time.Duration
		poll     time.Duration
		method   string
		out      string
	)
	cmd := &cobra.Command{
		Use:   "trace",
		Short: "Record a timing trace of Spotify's position against captured audio",
		Long: `Record a timing trace: every player observation and every captured audio block,
on one clock, while you play, skip, pause and seek in Spotify. At the end it
prints, per track change, the measured offset between the audio onset and the
position-based estimate (the pipeline latency), the position update step, and
any stale positions. Nothing is recorded to the library.`,
		Example: `  mixtape debug trace --duration 2m
  mixtape debug trace --capture device --out trace.ndjson`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.trace(cmd.Context(), duration, poll, method, out)
		},
	}
	cmd.Flags().DurationVar(&duration, "duration", time.Minute, "How long to trace")
	cmd.Flags().DurationVar(&poll, "poll", 100*time.Millisecond, "Player observation interval")
	cmd.Flags().StringVar(&method, "capture", "", "Capture method: auto, tap or device")
	cmd.Flags().StringVar(&out, "out", "", "Trace file (default ~/.mixtape/traces/<time>.ndjson)")
	return cmd
}

func (a *app) trace(ctx context.Context, duration, poll time.Duration, method, out string) error {
	o := config.Overrides{}
	if method != "" {
		o.Capture = &method
	}
	s, err := a.settings(o)
	if err != nil {
		return err
	}
	m, err := a.selectCapture(s)
	if err != nil {
		return err
	}
	src, err := a.deps.services.openCapture(ctx, captureRequest{Method: m, Device: s.Device})
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if out == "" {
		out = filepath.Join(a.home, "traces", a.deps.now().UTC().Format("20060102-150405")+".ndjson")
	}
	tw, err := createTrace(out)
	if err != nil {
		return err
	}
	defer tw.close()

	start := time.Now()
	format := src.Format()
	tw.write(traceLine{K: "meta", Rate: format.Rate, Chans: format.Channels, Src: src.Name(), PollM: poll.Milliseconds()})
	if err := src.Start(ctx, tw.audioSink(start, format)); err != nil {
		return err
	}
	a.out.Println("tracing for %s with %s at %d Hz into %s — play, skip, pause and seek in Spotify now (Ctrl-C ends early)",
		duration, src.Name(), format.Rate, out)
	a.traceSnapshots(ctx, tw, start, duration, poll)
	_ = src.Close() // no audio may arrive while the trace is flushed
	if err := tw.flush(); err != nil {
		return err
	}
	return a.out.Result(analyzeTrace(out), pathDoc{Path: out})
}

// traceSnapshots writes Spotify's snapshots until duration passes or a
// stop signal arrives.
func (a *app) traceSnapshots(ctx context.Context, tw *traceWriter, start time.Time, duration, poll time.Duration) {
	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	stopSigs := a.watchSignals(func(os.Signal) { cancel() })
	defer stopSigs()
	for snap := range a.deps.services.player().Watch(tctx, poll) {
		l := traceLine{
			K: "snap", T: snap.At.Sub(start).Nanoseconds(), State: string(snap.State), Pos: snap.Position,
			ID: snap.Track.URI, Name: snap.Track.Name, DurMS: snap.Track.Duration.Milliseconds(), Vol: snap.Volume,
		}
		if snap.Err != nil {
			l.Err = snap.Err.Error()
		}
		tw.write(l)
	}
}

// traceWriter writes trace lines from the capture and the player
// goroutines; the first write error sticks.
type traceWriter struct {
	mu    sync.Mutex
	f     *os.File
	w     *bufio.Writer
	enc   *json.Encoder
	err   error
	frame int64 // index of the next audio frame
}

// createTrace creates the trace file at path, and its folder.
func createTrace(path string) (*traceWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}
	f, err := os.Create(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("create trace: %w", err)
	}
	w := bufio.NewWriter(f)
	return &traceWriter{f: f, w: w, enc: json.NewEncoder(w)}, nil
}

func (tw *traceWriter) write(l traceLine) {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if err := tw.enc.Encode(l); err != nil && tw.err == nil {
		tw.err = err
	}
}

// audioSink traces each captured block: its time, first frame, length and
// loudest sample.
func (tw *traceWriter) audioSink(start time.Time, f capture.Format) func(capture.Block) {
	return func(b capture.Block) {
		peak := pcmPeak(b.PCM[:min(len(b.PCM), b.Frames*f.BytesPerFrame())])
		tw.mu.Lock()
		at := tw.frame
		tw.frame += int64(b.Frames)
		tw.mu.Unlock()
		tw.write(traceLine{K: "audio", T: b.At.Sub(start).Nanoseconds(), Frame: at, N: b.Frames, Peak: peak})
	}
}

// flush writes out what is buffered and reports the first error.
func (tw *traceWriter) flush() error {
	tw.mu.Lock()
	defer tw.mu.Unlock()
	if err := tw.w.Flush(); err != nil && tw.err == nil {
		tw.err = err
	}
	return tw.err
}

func (tw *traceWriter) close() { _ = tw.f.Close() }

// analyzeTrace summarizes a trace file: per playback start (a track change
// or a resume), the offset between the audio onset and the position-based
// start estimate, plus the position step and stale positions.
func analyzeTrace(path string) string {
	lines, rate := readTrace(path)
	if rate == 0 {
		return "trace " + path + ": no metadata"
	}
	var snaps, audio []traceLine
	for _, l := range lines {
		switch l.K {
		case "snap":
			snaps = append(snaps, l)
		case "audio":
			audio = append(audio, l)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "trace %s: %d snapshots, %d audio blocks at %d Hz", path, len(snaps), len(audio), rate)
	var steps []float64
	for i := 1; i < len(snaps); i++ {
		p, c := snaps[i-1], snaps[i]
		if c.State == "playing" && p.State == "playing" && c.ID == p.ID && c.Pos > p.Pos {
			steps = append(steps, c.Pos-p.Pos)
		}
		if c.State != "playing" || (p.State == "playing" && c.ID == p.ID) {
			continue // not a start
		}
		anchor, stale, ok := startAnchor(snaps, i, audio, rate)
		if !ok {
			continue
		}
		kind := "change"
		if c.ID == p.ID {
			kind = "resume"
		}
		// The audio starts where playback starts: at position 0 after a
		// change, at the paused position after a resume.
		estimate := anchor
		if kind == "resume" {
			estimate += p.Pos * float64(rate)
		}
		fmt.Fprintf(&b, "\n  %6.2fs %-6s %-32.32s stale=%d", float64(c.T)/1e9, kind, c.Name, stale)
		if onset := audioOnset(audio, p.T); onset >= 0 {
			fmt.Fprintf(&b, "  onset−estimate = %+.3fs", (onset-estimate)/float64(rate))
		}
	}
	if len(steps) > 0 {
		slices.Sort(steps)
		fmt.Fprintf(&b, "\n  position step: min %.3fs, median %.3fs, max %.3fs", steps[0], steps[len(steps)/2], steps[len(steps)-1])
	}
	return b.String()
}

// startAnchor estimates the frame of position 0 for the playback that
// starts at snaps[i]: the median, over its first two seconds, of each
// snapshot's frame minus its position. Positions a song that just started
// could not have reached are stale (the previous track's, reported under
// the new one) and counted instead; ok is false when none is left.
func startAnchor(snaps []traceLine, i int, audio []traceLine, rate int) (anchor float64, stale int, ok bool) {
	p, c := snaps[i-1], snaps[i]
	var anchors []float64
	for _, s := range snaps[i:] {
		if s.T-c.T >= int64(2*time.Second) || s.State != "playing" || s.ID != c.ID {
			break
		}
		if elapsed := float64(s.T-p.T) / 1e9; c.ID != p.ID && s.Pos > elapsed+0.3 {
			stale++
			continue
		}
		anchors = append(anchors, interpolateFrame(audio, s.T, rate)-s.Pos*float64(rate))
	}
	if len(anchors) == 0 {
		return 0, stale, false
	}
	slices.Sort(anchors)
	return anchors[len(anchors)/2], stale, true
}

func readTrace(path string) ([]traceLine, int) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return nil, 0
	}
	defer func() { _ = f.Close() }()
	var lines []traceLine
	rate := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l traceLine
		if json.Unmarshal(sc.Bytes(), &l) == nil {
			if l.K == "meta" {
				rate = l.Rate
			}
			lines = append(lines, l)
		}
	}
	return lines, rate
}

// interpolateFrame maps trace time t to a frame index using the audio
// blocks' timestamps.
func interpolateFrame(audio []traceLine, t int64, rate int) float64 {
	if len(audio) == 0 {
		return 0
	}
	i, _ := slices.BinarySearchFunc(audio, t, func(l traceLine, t int64) int {
		switch {
		case l.T < t:
			return -1
		case l.T > t:
			return 1
		}
		return 0
	})
	i = min(max(i-1, 0), len(audio)-1)
	a := audio[i]
	return float64(a.Frame) + float64(t-a.T)/1e9*float64(rate)
}

// audioOnset finds where sound starts after time from (the last
// observation before playback started): the first audible block after at
// least 200 ms of silence, confirmed when the sound turns loud. The silence
// may begin before from (a pause). Spotify fades a resumed song in, so the
// sound rises through moderate levels and can dip back to quiet on the
// way; only another 200 ms of silence means it died away and was not the
// start. -1 when there is none within 5 s.
func audioOnset(audio []traceLine, from int64) float64 {
	const loud, quiet = 300, 16
	const silence = int64(200 * time.Millisecond)
	quietSince := int64(-1) // start of the current run of quiet blocks
	rise := -1.0            // first frame of sound after a long enough silence
	for _, a := range audio {
		if a.T-from > int64(5*time.Second) {
			return -1
		}
		if a.Peak <= quiet {
			if quietSince < 0 {
				quietSince = a.T
			}
			if rise >= 0 && a.T-quietSince >= silence {
				rise = -1 // the sound died away again
			}
			continue
		}
		if rise < 0 && quietSince >= 0 && a.T-quietSince >= silence && a.T >= from {
			rise = float64(a.Frame)
		}
		quietSince = -1
		if rise >= 0 && a.Peak >= loud {
			return rise
		}
	}
	return -1
}
