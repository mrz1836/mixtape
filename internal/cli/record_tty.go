package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// ttyUI is the interactive view of a recording: a status line redrawn in
// place, log lines above it, and single-key controls read in raw mode.
type ttyUI struct {
	a       *app
	eng     *engine.Engine
	out     io.Writer
	restore func()
	mu      sync.Mutex
	status  string
	done    chan struct{}
	once    sync.Once
}

// statusEvery is how often the status line is redrawn.
const statusEvery = 250 * time.Millisecond

// startTTY puts the terminal in raw mode and starts the key reader and
// the status line. Ctrl-C arrives as a byte in raw mode: the first one
// stops gracefully, the second forces the quit.
func startTTY(a *app, eng *engine.Engine, controls chan<- engine.Control, force context.CancelFunc) (*ttyUI, error) {
	fd := int(a.deps.stdinFile.Fd())
	old, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	u := &ttyUI{
		a: a, eng: eng, out: a.deps.stdout, done: make(chan struct{}),
		restore: func() { _ = term.Restore(fd, old) },
	}
	u.log(a.out.paint(colorDim, "keys: [space] pause/resume  [n] skip song  [q] quit"))
	go u.readKeys(controls, force)
	go u.redraw()
	return u, nil
}

// close restores the terminal. It is idempotent.
func (u *ttyUI) close() {
	u.once.Do(func() {
		close(u.done)
		u.mu.Lock()
		defer u.mu.Unlock()
		_, _ = io.WriteString(u.out, "\r\x1b[K")
		u.restore()
	})
}

func (u *ttyUI) readKeys(controls chan<- engine.Control, force context.CancelFunc) {
	buf := make([]byte, 16)
	interrupts := 0
	for {
		n, err := u.a.deps.stdin.Read(buf)
		if err != nil {
			return
		}
		for _, b := range buf[:n] {
			var c engine.Control
			switch b {
			case ' ':
				c = engine.ControlPlayPause
			case 'n', 'N':
				c = engine.ControlNext
			case 'q', 'Q':
				c = engine.ControlStop
			case 0x03: // Ctrl-C
				interrupts++
				if interrupts >= 2 {
					force()
					continue
				}
				c = engine.ControlStop
			default:
				continue
			}
			if c == engine.ControlStop {
				u.log(u.a.out.paint(colorYellow, "stopping after this song is settled… (Ctrl-C again to force)"))
			}
			select {
			case controls <- c:
			default:
			}
		}
	}
}

func (u *ttyUI) redraw() {
	t := time.NewTicker(statusEvery)
	defer t.Stop()
	for {
		select {
		case <-u.done:
			return
		case <-t.C:
			u.setStatus(statusLine(u.eng.Progress(), u.a.out, u.width()))
		}
	}
}

func (u *ttyUI) width() int {
	if u.a.deps.stdinFile == nil {
		return 100
	}
	if w, _, err := term.GetSize(int(u.a.deps.stdinFile.Fd())); err == nil && w > 20 {
		return w - 1
	}
	return 100
}

func (u *ttyUI) setStatus(line string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	select {
	case <-u.done:
		return
	default:
	}
	u.status = line
	_, _ = io.WriteString(u.out, "\r\x1b[K"+line)
}

// log prints a line above the status line. Raw mode disables output
// post-processing, so lines end in \r\n.
func (u *ttyUI) log(line string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	line = strings.ReplaceAll(line, "\n", "\r\n")
	_, _ = io.WriteString(u.out, "\r\x1b[K"+line+"\r\n"+u.status)
}

// statusLine renders, for example:
//
//	⏺ REC 1:12/3:45 │ Scorpions — Still Loving You │ 3 of 12 · ~32m left
func statusLine(p engine.Progress, o *Output, width int) string {
	var indicator string
	switch {
	case p.Phase == "stopping":
		indicator = o.paint(colorYellow, "■ STOP")
	case p.State == spotify.StatePlaying && p.Recording:
		indicator = o.paint(colorRed, "⏺ REC ")
	case p.State == spotify.StatePlaying:
		indicator = o.paint(colorDim, "▷ SKIP")
	default:
		indicator = o.paint(colorYellow, "⏸ WAIT")
	}
	label := "waiting for Spotify…"
	clock := ""
	if p.Track != nil {
		label = songLabel(p.Track.Artist, p.Track.Title)
		clock = " " + mmss(seconds(p.Position)) + "/" + mmss(seconds(p.Duration))
	}
	done := p.Counts.Recorded + p.Counts.Copied + p.Counts.Skipped
	parts := []string{indicator + clock, label}
	if p.Mode != engine.ModeLive && p.Counts.Total > 0 {
		progress := fmt.Sprintf("%d of %d", done, p.Counts.Total)
		if p.ETA > 0 {
			progress += " · ~" + roundETA(p.ETA) + " left"
		}
		parts = append(parts, progress)
	} else {
		parts = append(parts, fmt.Sprintf("%d recorded", p.Counts.Recorded))
	}
	return truncateDisplay(strings.Join(parts, " │ "), width)
}

// eventLine renders an event as a log line ("" for none).
func eventLine(ev engine.Event, o *Output) string {
	t := ev.Track
	switch ev.Type {
	case engine.EventSessionStarted:
		return o.paint(colorDim, fmt.Sprintf("mixtape: %s session, capture %s, library %s", ev.Mode, ev.Capture, ev.Library))
	case engine.EventPlan:
		return planLine(ev.Items)
	case engine.EventTrackStarted:
		return "▶ " + songLabel(t.Artist, t.Title) + o.paint(colorDim, " ("+mmss(ms(t.DurationMS))+")")
	case engine.EventTrackSaved:
		mark, note := o.paint(colorGreen, "✓ saved"), ""
		if t.Partial {
			mark, note = o.paint(colorYellow, "◐ partial"), o.paint(colorDim, " (kept with --keep-partial)")
		}
		return fmt.Sprintf("%s %s → %s%s", mark, songLabel(t.Artist, t.Title), t.File, note)
	case engine.EventTrackCopied:
		return fmt.Sprintf("%s %s → %s", o.paint(colorGreen, "⧉ copied"), songLabel(t.Artist, t.Title), t.File)
	case engine.EventTrackSkipped:
		return o.paint(colorDim, "· skipped "+songLabel(t.Artist, t.Title)+" ("+ev.Reason+")")
	case engine.EventTrackDiscarded:
		return o.paint(colorYellow, "✗ discarded "+songLabel(t.Artist, t.Title)+": "+ev.Reason)
	case engine.EventTrackFailed:
		return o.paint(colorRed, "✗ failed "+songLabel(t.Artist, t.Title)+": "+ev.Reason)
	case engine.EventWarning:
		return o.paint(colorYellow, "! "+ev.Message)
	case engine.EventBoundary:
		if !o.Verbose() || ev.OffsetSeconds == nil {
			return ""
		}
		return o.paint(colorDim, fmt.Sprintf("  boundary %s: offset %+.3fs, drift %.3fs, runs %d %s",
			t.Title, *ev.OffsetSeconds, deref(ev.DriftSeconds), ev.Runs, ev.Reason))
	}
	return ""
}

func planLine(items []engine.PlanItem) string {
	n := map[string]int{}
	for _, it := range items {
		n[it.Action]++
	}
	if len(items) == 0 {
		return "recording whatever Spotify plays"
	}
	return fmt.Sprintf("plan: %d to record, %d to copy, %d already done", n["record"], n["copy"], n["skip"])
}

// summaryLine is the last line of a text session.
func summaryLine(res engine.Result, o *Output) string {
	c := res.Counts
	line := fmt.Sprintf("%s: %d recorded, %d copied, %d skipped", res.Outcome, c.Recorded, c.Copied, c.Skipped)
	if c.Failed > 0 {
		line += fmt.Sprintf(", %d failed", c.Failed)
	}
	if c.Remaining > 0 {
		line += fmt.Sprintf(", %d remaining", c.Remaining)
	}
	color := colorGreen
	if res.Outcome != engine.OutcomeComplete {
		color = colorYellow
	}
	return o.paint(color, line)
}

func songLabel(artist, title string) string {
	if artist == "" {
		return title
	}
	return artist + " — " + title
}

// mmss formats a duration as m:ss (or h:mm:ss past an hour).
func mmss(d time.Duration) string {
	s := max(int(d.Round(time.Second).Seconds()), 0)
	if s >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", s/3600, s/60%60, s%60)
	}
	return fmt.Sprintf("%d:%02d", s/60, s%60)
}

func roundETA(d time.Duration) string {
	if d < time.Minute {
		return "1m"
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// ago renders how long ago something happened.
func ago(d time.Duration) string {
	if d < time.Minute {
		return "just now"
	}
	return roundETA(d) + " ago"
}

func seconds(s float64) time.Duration { return time.Duration(s * float64(time.Second)) }

func ms(v int64) time.Duration { return time.Duration(v) * time.Millisecond }

func deref(f *float64) float64 {
	if f == nil {
		return 0
	}
	return *f
}

// truncateDisplay shortens s to at most width visible characters,
// ignoring SGR color sequences.
func truncateDisplay(s string, width int) string {
	visible := utf8.RuneCountInString(ansiSGR.ReplaceAllString(s, ""))
	if visible <= width {
		return s
	}
	plain := []rune(ansiSGR.ReplaceAllString(s, ""))
	return string(plain[:max(width-1, 0)]) + "…"
}
