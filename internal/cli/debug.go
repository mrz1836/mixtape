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
	src, err := a.deps.services.openCapture(captureRequest{Method: m, Device: s.Device})
	if err != nil {
		return err
	}
	defer func() { _ = src.Close() }()
	if out == "" {
		out = filepath.Join(a.home, "traces", a.deps.now().UTC().Format("20060102-150405")+".ndjson")
	}
	if mkErr := os.MkdirAll(filepath.Dir(out), 0o700); mkErr != nil {
		return mkErr
	}
	f, err := os.Create(filepath.Clean(out))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriter(f)
	enc := json.NewEncoder(w)
	var (
		mu       sync.Mutex
		writeErr error // first trace write failure, guarded by mu
	)
	write := func(l traceLine) {
		mu.Lock()
		defer mu.Unlock()
		if encErr := enc.Encode(l); encErr != nil && writeErr == nil {
			writeErr = encErr
		}
	}

	start := time.Now()
	format := src.Format()
	write(traceLine{K: "meta", Rate: format.Rate, Chans: format.Channels, Src: src.Name(), PollM: poll.Milliseconds()})
	var frame int64
	err = src.Start(ctx, func(b capture.Block) {
		peakVal := pcmPeak(b.PCM[:min(len(b.PCM), b.Frames*format.BytesPerFrame())])
		mu.Lock()
		at := frame
		frame += int64(b.Frames)
		mu.Unlock()
		write(traceLine{K: "audio", T: b.At.Sub(start).Nanoseconds(), Frame: at, N: b.Frames, Peak: peakVal})
	})
	if err != nil {
		return err
	}

	tctx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	a.out.Println("tracing for %s with %s at %d Hz into %s — play, skip, pause and seek in Spotify now (Ctrl-C ends early)",
		duration, src.Name(), format.Rate, out)
	sigs, stopSigs := a.deps.services.signals()
	defer stopSigs()
	snaps := a.deps.services.player().Watch(tctx, poll)
	for done := false; !done; {
		select {
		case <-sigs:
			done = true
		case snap, ok := <-snaps:
			if !ok {
				done = true
				continue
			}
			l := traceLine{
				K: "snap", T: snap.At.Sub(start).Nanoseconds(), State: string(snap.State), Pos: snap.Position,
				ID: snap.Track.URI, Name: snap.Track.Name, DurMS: snap.Track.Duration.Milliseconds(), Vol: snap.Volume,
			}
			if snap.Err != nil {
				l.Err = snap.Err.Error()
			}
			write(l)
		}
	}
	_ = src.Close()
	mu.Lock()
	ferr := w.Flush()
	if writeErr != nil {
		ferr = writeErr
	}
	mu.Unlock()
	if ferr != nil {
		return ferr
	}
	return a.out.Result(analyzeTrace(out), pathDoc{Path: out})
}

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
	frameAt := func(t int64) float64 { return interpolateFrame(audio, t, rate) }

	var b strings.Builder
	fmt.Fprintf(&b, "trace %s: %d snapshots, %d audio blocks at %d Hz", path, len(snaps), len(audio), rate)
	var steps []float64
	for i := 1; i < len(snaps); i++ {
		p, c := snaps[i-1], snaps[i]
		if c.State == "playing" && p.State == "playing" && c.ID == p.ID && c.Pos > p.Pos {
			steps = append(steps, c.Pos-p.Pos)
		}
		started := c.State == "playing" && (p.State != "playing" || c.ID != p.ID)
		if !started {
			continue
		}
		var anchors []float64
		stale := 0
		for j := i; j < len(snaps) && snaps[j].T-c.T < int64(2*time.Second) && snaps[j].State == "playing" && snaps[j].ID == c.ID; j++ {
			elapsed := float64(snaps[j].T-p.T) / 1e9
			if c.ID != p.ID && snaps[j].Pos > elapsed+0.3 {
				stale++
				continue
			}
			anchors = append(anchors, frameAt(snaps[j].T)-snaps[j].Pos*float64(rate))
		}
		if len(anchors) == 0 {
			continue
		}
		slices.Sort(anchors)
		anchor := anchors[len(anchors)/2]
		onset := audioOnset(audio, p.T)
		kind := "change"
		if c.ID == p.ID {
			kind = "resume"
		}
		fmt.Fprintf(&b, "\n  %6.2fs %-6s %-32.32s stale=%d", float64(c.T)/1e9, kind, c.Name, stale)
		if onset >= 0 {
			fmt.Fprintf(&b, "  onset−estimate = %+.3fs", (onset-anchor)/float64(rate))
		}
	}
	if len(steps) > 0 {
		slices.Sort(steps)
		fmt.Fprintf(&b, "\n  position step: min %.3fs, median %.3fs, max %.3fs", steps[0], steps[len(steps)/2], steps[len(steps)-1])
	}
	return b.String()
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

// audioOnset finds the first loud frame after at least 200 ms of silence
// following time from; -1 when there is none within 5 s.
func audioOnset(audio []traceLine, from int64) float64 {
	const loud, quiet = 300, 16
	var silentSince int64 = -1
	for _, a := range audio {
		if a.T < from {
			continue
		}
		if a.T-from > int64(5*time.Second) {
			return -1
		}
		switch {
		case a.Peak <= quiet:
			if silentSince < 0 {
				silentSince = a.T
			}
		case a.Peak >= loud && silentSince >= 0 && a.T-silentSince >= int64(200*time.Millisecond):
			return float64(a.Frame)
		default:
			silentSince = -1
		}
	}
	return -1
}
