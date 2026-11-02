package engine

import (
	"context"
	"encoding/binary"
	"fmt"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spool"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// startCapture opens the spool and starts the capture source feeding it.
func (e *Engine) startCapture(ctx context.Context) error {
	src := e.cfg.Source
	if src == nil {
		return fmt.Errorf("%w: no capture source", errConfig)
	}
	sp, err := spool.Open(e.cfg.SpoolDir, src.Format())
	if err != nil {
		return fmt.Errorf("open spool: %w", err)
	}
	e.spool = sp
	e.capStart = e.now()
	if err := src.Start(ctx, e.sink); err != nil {
		_ = sp.Close()
		e.spool = nil
		return fmt.Errorf("start capture (%s): %w", src.Name(), err)
	}
	return nil
}

// stopCapture stops the source and deletes the spool.
func (e *Engine) stopCapture() {
	if e.cfg.Source != nil {
		_ = e.cfg.Source.Close()
	}
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool != nil {
		_ = e.spool.Close()
		e.spool = nil
	}
}

// sink receives captured blocks on the capture goroutine: it spools them
// and tracks when audio was last above the silence floor.
func (e *Engine) sink(b capture.Block) {
	if peak(b.PCM[:min(len(b.PCM), b.Frames*4)]) > silenceFloor {
		e.loudAt.Store(int64(b.At.Sub(e.capStart)))
	}
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool == nil || e.cfg.Source.Format() != e.spool.Format() {
		return // a format change is being handled; the take is already invalid
	}
	_ = e.spool.Append(b)
}

// peak returns the largest absolute sample in interleaved PCM16LE data.
func peak(pcm []byte) int {
	m := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(binary.LittleEndian.Uint16(pcm[i:]))
		if v >= 1<<15 {
			v = 1<<16 - v // two's complement: the magnitude of a negative sample
		}
		m = max(m, v)
	}
	return m
}

// spoolFrames is the number of frames captured so far.
func (e *Engine) spoolFrames() int64 {
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool == nil {
		return 0
	}
	return e.spool.Frames()
}

// onCaptureEvent invalidates the songs a capture problem touched.
func (e *Engine) onCaptureEvent(ev capture.Event) {
	reason := fmt.Sprintf("capture problem (%s)", ev.Kind)
	if ev.Detail != "" {
		reason += ": " + ev.Detail
	}
	if t := e.cur; t != nil && t.recording && t.invalid == "" {
		t.invalid = reason
	}
	for _, t := range e.finals {
		if t.invalid == "" {
			t.invalid = reason
		}
	}
	switch ev.Kind {
	case capture.EventStopped:
		e.fail(fmt.Errorf("%w: %s", ErrCaptureStopped, ev.Detail))
	case capture.EventFormatChanged:
		e.reopenSpool()
	case capture.EventDeviceChanged, capture.EventProcessChanged, capture.EventOverflow:
	}
	e.warn(reason)
}

// reopenSpool replaces the spool after a format change. Every song that
// touched the old spool has already been invalidated.
func (e *Engine) reopenSpool() {
	e.finalizeReady(true)
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool != nil {
		_ = e.spool.Close()
	}
	sp, err := spool.Open(e.cfg.SpoolDir, e.cfg.Source.Format())
	if err != nil {
		e.spool = nil
		e.fatal = fmt.Errorf("reopen spool: %w", err)
		e.stopping = true
		return
	}
	e.spool = sp
}

// checkSilence enforces the silence invariant: Spotify playing a song for
// silenceWindow while the capture hears nothing. Before the first audible
// sample of the session that means capture is not working at all (wrong
// routing, or no permission) and the session ends; afterwards only the
// song is invalidated.
func (e *Engine) checkSilence(now time.Time) {
	t := e.cur
	if t == nil || !t.recording || e.last.State != spotify.StatePlaying || e.cfg.Source == nil {
		return
	}
	quietSince := t.startedAt
	loud := e.loudAt.Load()
	if loud >= 0 {
		if at := e.capStart.Add(time.Duration(loud)); at.After(quietSince) {
			quietSince = at
		}
	}
	if now.Sub(quietSince) < silenceWindow || !advanced(t, quietSince, silenceWindow) {
		return
	}
	if loud < 0 {
		if e.cfg.Source.Method() == capture.MethodTap {
			e.fail(ErrSilentTap)
		} else {
			e.fail(ErrSilentDevice)
		}
		return
	}
	if t.invalid == "" {
		t.invalid = "no audio was captured for 10 s while the song played"
	}
}

// advanced reports whether playback position moved at least d since t0.
func advanced(t *take, t0 time.Time, d time.Duration) bool {
	first, last := -1.0, -1.0
	for _, o := range t.obs {
		if !o.Playing || o.Position < 0 || o.At.Before(t0) {
			continue
		}
		if first < 0 {
			first = o.Position
		}
		last = o.Position
	}
	return first >= 0 && last-first >= d.Seconds()*0.9
}

// pruneSpool drops audio no open song can need.
func (e *Engine) pruneSpool() {
	keepFrom := e.now().Add(-spoolKeep)
	if t := e.cur; t != nil && t.recording && t.startedAt.Before(keepFrom) {
		keepFrom = t.startedAt.Add(-spoolKeep)
	}
	for _, t := range e.finals {
		if t.startedAt.Add(-spoolKeep).Before(keepFrom) {
			keepFrom = t.startedAt.Add(-spoolKeep)
		}
	}
	e.spoolMu.Lock()
	defer e.spoolMu.Unlock()
	if e.spool != nil {
		_ = e.spool.Prune(e.spool.FrameAt(keepFrom))
	}
}

// setupSpotify saves the player settings, then turns shuffle off (and
// repeat on to follow a playlist, and the volume up when recording muted).
func (e *Engine) setupSpotify(ctx context.Context) error {
	snap := e.cfg.Player.Poll(ctx)
	if snap.Err != nil {
		return snap.Err
	}
	e.last = snap
	st := session.SpotifyState{}
	if snap.Shuffle {
		st.Original.Shuffle, st.Set.Shuffle = ptr(true), ptr(false)
	}
	if e.plan.Mode == ModeFollow && !snap.Repeat {
		st.Original.Repeat, st.Set.Repeat = ptr(false), ptr(true)
	}
	if e.opts.Mute && snap.Volume != 100 {
		st.Original.Volume, st.Set.Volume = ptr(snap.Volume), ptr(100)
	}
	if e.plan.Mode == ModeLive {
		st = session.SpotifyState{} // live mode leaves the listener's settings alone
	}
	if st.Set.Empty() {
		return nil
	}
	if e.cfg.SaveSpotify != nil {
		if err := e.cfg.SaveSpotify(st); err != nil {
			return fmt.Errorf("save spotify settings: %w", err)
		}
	}
	e.spotifyState = st
	if v := st.Set.Shuffle; v != nil {
		e.command(ctx, "shuffle", func(c context.Context) error { return e.cfg.Player.SetShuffle(c, *v) })
	}
	if v := st.Set.Repeat; v != nil {
		e.command(ctx, "repeat", func(c context.Context) error { return e.cfg.Player.SetRepeat(c, *v) })
	}
	if v := st.Set.Volume; v != nil {
		e.command(ctx, "volume", func(c context.Context) error { return e.cfg.Player.SetVolume(c, *v) })
	}
	return nil
}

// restoreSpotify puts back the settings the session changed.
func (e *Engine) restoreSpotify(ctx context.Context) {
	if !e.spotifyState.NeedsRestore() {
		return
	}
	c, cancel := context.WithTimeout(ctx, cleanupTimeout)
	defer cancel()
	if _, err := session.Restore(c, e.cfg.Player, e.spotifyState); err != nil {
		e.warn("could not restore Spotify settings: " + err.Error())
		return
	}
	e.spotifyState.Restored = true
	if e.cfg.SaveSpotify != nil {
		_ = e.cfg.SaveSpotify(e.spotifyState)
	}
}

func ptr[T any](v T) *T { return &v }
