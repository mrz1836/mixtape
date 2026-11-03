//go:build darwin && cgo

// Package tap records Spotify's audio with a Core Audio process tap
// (macOS 14.2 and later): nothing to install, no audio routing, only
// Spotify's sound, and optionally muted on the speakers.
//
// The permission to record system audio belongs to the app that runs
// mixtape (Terminal, iTerm, an IDE), so it cannot be granted over ssh or
// to a launchd job; those use the capture/device backend.
package tap

/*
#cgo CFLAGS: -fobjc-arc -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework CoreAudio -framework AudioToolbox -framework Foundation
#include <stdlib.h>
#include "ctap.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/spotify"
)

const (
	ringSeconds    = 4.0
	waitInterval   = 250 * time.Millisecond
	retryInterval  = time.Second
	maxProcesses   = 64
	maxBundleBytes = 512
	channels       = 2
)

var (
	// ErrNoProcess reports that Spotify has no audio process to tap.
	ErrNoProcess = errors.New("spotify has no audio process to tap (is it running?)")
	// ErrCreateTap reports a Core Audio failure while building the tap.
	ErrCreateTap = errors.New("could not create the process tap")
)

// Available reports whether this Mac can run process taps.
func Available() capture.TapStatus {
	raw, err := syscall.Sysctl("kern.osproductversion")
	v, ok := parseOSVersion(raw)
	switch {
	case err != nil || !ok:
		return capture.TapStatus{Reason: "could not read the macOS version"}
	case !v.supportsTaps():
		return capture.TapStatus{Reason: fmt.Sprintf("macOS %s is older than 14.2", v)}
	case C.mt_tap_supported() == 0:
		return capture.TapStatus{Reason: "this build of mixtape has no process tap support"}
	}
	return capture.TapStatus{Supported: true}
}

// Process is one audio process object.
type Process struct {
	ObjectID uint32
	PID      int
	BundleID string
}

// SpotifyProcesses lists the audio process objects of Spotify.
func SpotifyProcesses() ([]Process, error) {
	if !Available().Supported {
		return nil, capture.ErrTapUnsupported
	}
	return processes(spotify.BundleID)
}

// processes lists the audio process objects whose bundle ID starts with
// prefix. The caller has checked that taps are available.
func processes(prefix string) ([]Process, error) {
	cprefix := C.CString(prefix)
	defer C.free(unsafe.Pointer(cprefix))
	var (
		ids   [maxProcesses]C.AudioObjectID
		pids  [maxProcesses]C.int32_t
		count C.int
	)
	if st := C.mt_list_processes(cprefix, &ids[0], &pids[0], maxProcesses, &count); st != 0 {
		return nil, fmt.Errorf("%w: list audio processes: %s", ErrCreateTap, osStatus(int32(st)))
	}
	out := make([]Process, 0, int(count))
	buf := (*C.char)(C.malloc(maxBundleBytes))
	defer C.free(unsafe.Pointer(buf))
	for i := range int(count) {
		C.mt_process_bundle(ids[i], buf, maxBundleBytes)
		out = append(out, Process{ObjectID: uint32(ids[i]), PID: int(pids[i]), BundleID: C.GoString(buf)})
	}
	return out, nil
}

// Options configures Open.
type Options struct {
	// Mute silences Spotify on the speakers while it is being recorded.
	Mute bool
	// BundlePrefix selects the processes to tap; empty means Spotify
	// (spotify.BundleID, which its helper processes extend).
	BundlePrefix string
	// Wait is how long Open waits for the processes to appear.
	Wait time.Duration
	// Bits is the depth of the PCM delivered: 16 or 24 (0 means 16, and
	// Format reports it as given).
	Bits int
	// OutputRate, when set, runs the default output device at this rate
	// in Hz while the tap records (Spotify then plays its 44.1 kHz streams
	// without resampling them), and puts the original back on Close. It
	// never fails Open: a rate the device does not offer, or a switch that
	// does not hold, is a warning, and the rate stays as it was.
	OutputRate int
	// SaveRestore persists what the switch must undo, before switching
	// (and the zero record once undone), so a crash can be repaired.
	SaveRestore func(capture.OutputRestore) error
	// Pending is a switch an interrupted session left in place, taken
	// over when it is the one OutputRate asks for (see
	// capture.OutputSwitch.Begin).
	Pending *capture.OutputRestore
	// Warn receives the output switch's warnings; nil discards them.
	Warn func(msg string)
	// SessionID names the session in the restore record.
	SessionID string
}

// Source records the tapped processes. It implements capture.Source.
type Source struct {
	opts Options

	life   capture.Lifecycle
	events *capture.Emitter

	mu     sync.Mutex
	tap    unsafe.Pointer // retained MTTap, owned by the drain once started
	procs  []uint32
	format capture.Format

	output *capture.OutputSwitch // nil unless Options.OutputRate is set
}

var _ capture.Source = (*Source)(nil)

// Open builds the tap on Spotify's audio processes, waiting up to o.Wait
// for them to appear (or ctx to end). Audio flows to the sink once Start
// is called.
func Open(ctx context.Context, o Options) (*Source, error) {
	if st := Available(); !st.Supported {
		return nil, fmt.Errorf("%w: %s", capture.ErrTapUnsupported, st.Reason)
	}
	if o.BundlePrefix == "" {
		o.BundlePrefix = spotify.BundleID
	}
	procs, err := waitForProcesses(ctx, o.BundlePrefix, o.Wait)
	if err != nil {
		return nil, err
	}
	s := &Source{opts: o, events: capture.NewEmitter()}
	switched := s.switchOutput(ctx)
	if err := s.build(procs); err != nil {
		s.restoreOutput(ctx)
		return nil, err
	}
	if want := o.OutputRate; switched && s.Format().Rate != want {
		// The aggregate can come up at the old rate when it is built right
		// after the switch: rebuild it once, quietly.
		s.closeTap()
		if err := s.build(procs); err != nil {
			s.restoreOutput(ctx)
			return nil, err
		}
		if got := s.Format().Rate; got != want {
			s.warn(fmt.Sprintf("the tap records at %d Hz although the output was switched to %d Hz", got, want))
		}
	}
	return s, nil
}

// switchOutput runs the output at Options.OutputRate, and reports whether
// it switched (or took over an interrupted session's switch).
func (s *Source) switchOutput(ctx context.Context) bool {
	if s.opts.OutputRate <= 0 {
		return false
	}
	s.output = &capture.OutputSwitch{
		Control: OutputControl(), Save: s.opts.SaveRestore, Warn: s.opts.Warn, SessionID: s.opts.SessionID,
	}
	return s.output.Begin(ctx, s.opts.OutputRate, s.opts.Pending)
}

// restoreOutput puts the output's rate back, if Open switched it. It
// finishes even when ctx ends (capture.OutputSwitch.End).
func (s *Source) restoreOutput(ctx context.Context) {
	if s.output != nil {
		s.output.End(ctx)
	}
}

// closeTap destroys the current tap, if any.
func (s *Source) closeTap() {
	s.mu.Lock()
	t := s.tap
	s.tap = nil
	s.mu.Unlock()
	if t != nil {
		C.mt_tap_close(t)
	}
}

func (s *Source) warn(msg string) {
	if s.opts.Warn != nil {
		s.opts.Warn(msg)
	}
}

// processIDs lists the sorted object IDs of the processes to tap; none is
// ErrNoProcess.
func processIDs(prefix string) ([]uint32, error) {
	procs, err := processes(prefix)
	if err != nil {
		return nil, err
	}
	if len(procs) == 0 {
		return nil, ErrNoProcess
	}
	ids := make([]uint32, 0, len(procs))
	for _, p := range procs {
		ids = append(ids, p.ObjectID)
	}
	slices.Sort(ids)
	return ids, nil
}

// waitForProcesses lists the processes to tap, asking again every
// waitInterval for up to wait while there are none.
func waitForProcesses(ctx context.Context, prefix string, wait time.Duration) ([]uint32, error) {
	deadline := time.Now().Add(wait)
	for {
		ids, err := processIDs(prefix)
		if !errors.Is(err, ErrNoProcess) || !time.Now().Before(deadline) {
			return ids, err
		}
		t := time.NewTimer(waitInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

// build creates the C tap for procs and records its format. The caller
// holds no lock: build runs from Open or from the drain goroutine.
func (s *Source) build(procs []uint32) error {
	ids := make([]C.AudioObjectID, len(procs))
	for i, p := range procs {
		ids[i] = C.AudioObjectID(p)
	}
	mute := C.int(0)
	if s.opts.Mute {
		mute = 1
	}
	var (
		status C.OSStatus
		stage  *C.char
	)
	t := C.mt_tap_open(&ids[0], C.int(len(ids)), mute, C.double(ringSeconds), &status, &stage)
	if t == nil {
		return fmt.Errorf("%w: %s: %s", ErrCreateTap, C.GoString(stage), osStatus(int32(status)))
	}
	rate := float64(C.mt_tap_rate(t))
	if rate <= 0 || rate > math.MaxInt32 {
		C.mt_tap_close(t)
		return fmt.Errorf("%w: unusable sample rate %.0f Hz", ErrCreateTap, rate)
	}
	s.mu.Lock()
	s.tap, s.procs = t, procs
	s.format = capture.Format{Rate: int(math.Round(rate)), Channels: channels, Bits: s.opts.Bits}
	s.mu.Unlock()
	return nil
}

// Name describes the source.
func (s *Source) Name() string {
	if s.opts.Mute {
		return "process tap (Spotify, muted)"
	}
	return "process tap (Spotify)"
}

// Method is capture.MethodTap.
func (s *Source) Method() capture.Method { return capture.MethodTap }

// Format is the current capture format; it changes after an
// EventFormatChanged.
func (s *Source) Format() capture.Format {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.format
}

// Events reports rebuilds, format changes and overflows.
func (s *Source) Events() <-chan capture.Event { return s.events.Events() }

// Start delivers blocks to sink from a drain goroutine.
func (s *Source) Start(ctx context.Context, sink func(capture.Block)) error {
	return s.life.Start(ctx, nil, func(dctx context.Context) { s.drain(dctx, sink) })
}

// drain runs until ctx ends: it reads the ring, stamps blocks, answers
// Core Audio's change notifications by rebuilding the tap, and reports
// overflows.
func (s *Source) drain(ctx context.Context, sink func(capture.Block)) {
	clock := sampleClock()
	tick := time.NewTicker(capture.DrainInterval)
	defer tick.Stop()
	var buf drainBuffers
	var nextRetry time.Time
	for {
		s.mu.Lock()
		t, format := s.tap, s.format
		s.mu.Unlock()

		if t == nil {
			// A rebuild failed (Spotify quit?): retry until it comes back.
			if time.Now().After(nextRetry) {
				nextRetry = time.Now().Add(retryInterval)
				s.rebuild(capture.EventProcessChanged, "Spotify's audio process came back")
			}
		} else {
			s.read(t, format, clock, &buf, sink)
			s.handleEvents(t)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// drainBuffers are the drain's reusable buffers: the Float32 samples read
// from the ring, and the PCM they convert to.
type drainBuffers struct {
	samples []float32
	pcm     []byte
}

// read drains the ring of tap t into sink, converting the Float32 samples
// to f's depth.
func (s *Source) read(t unsafe.Pointer, f capture.Format, clock clockPair, buf *drainBuffers, sink func(capture.Block)) {
	ring := C.mt_tap_ring(t)
	maxFrames := uint64(ring.capacity)
	if need := int(maxFrames) * f.Channels; cap(buf.samples) < need {
		buf.samples = make([]float32, need)
		buf.pcm = make([]byte, 0, need*f.BytesPerSample())
	}
	for {
		n, first := readRing(ring, buf.samples, maxFrames)
		if n == 0 {
			break
		}
		var at time.Time
		var af, ah C.uint64_t
		if C.mt_ring_anchor(ring, &af, &ah) == 1 {
			at = clock.at(uint64(ah)).Add(-f.Duration(int64(uint64(af)) - int64(first)))
		}
		buf.pcm = capture.ConvertF32(buf.pcm[:0], buf.samples[:int(n)*f.Channels], f.SampleBits())
		sink(capture.Block{PCM: buf.pcm, Frames: int(n), At: at})
	}
	if n := uint64(C.mt_ring_take_overflows(ring)); n > 0 {
		s.events.Emit(capture.EventOverflow, fmt.Sprintf("%d tap IO cycles dropped: the reader fell behind", n))
	}
}

// handleEvents answers the listeners' change bits.
func (s *Source) handleEvents(t unsafe.Pointer) {
	bits := uint32(C.mt_tap_take_events(t))
	switch {
	case bits&C.MT_EVENT_OUTPUT != 0:
		s.rebuild(capture.EventDeviceChanged, "the default output device changed")
	case bits&C.MT_EVENT_RATE != 0:
		s.rebuild(capture.EventFormatChanged, "the output sample rate changed")
	case bits&C.MT_EVENT_PROCESSES != 0:
		procs, err := processIDs(s.opts.BundlePrefix)
		s.mu.Lock()
		same := err == nil && slices.Equal(procs, s.procs)
		s.mu.Unlock()
		if !same {
			s.rebuild(capture.EventProcessChanged, "Spotify's audio processes changed")
		}
	}
}

// rebuild tears the tap down and builds a new one on Spotify's current
// processes, then reports kind. On failure the tap stays nil and drain
// retries.
func (s *Source) rebuild(kind capture.EventKind, detail string) {
	s.mu.Lock()
	old, oldRate := s.tap, s.format.Rate
	s.tap = nil
	s.mu.Unlock()
	if old != nil {
		C.mt_tap_close(old)
	}
	procs, err := processIDs(s.opts.BundlePrefix)
	if err == nil {
		err = s.build(procs)
	}
	if err != nil {
		if old != nil {
			s.events.Emit(kind, detail+"; capture paused: "+err.Error())
		}
		return
	}
	// What changed is decided by the rates: a new output device can run at
	// another rate (headphones at 44.1 kHz after speakers at 48 kHz), and a
	// rate event can leave the rate as it was.
	switch rate := s.Format().Rate; {
	case rate != oldRate:
		kind = capture.EventFormatChanged
		detail += fmt.Sprintf(" (%d Hz → %d Hz)", oldRate, rate)
	case kind == capture.EventFormatChanged:
		kind = capture.EventDeviceChanged
	}
	s.events.Emit(kind, detail)
}

// Close stops capture, destroys the tap and puts back the output rate Open
// switched. It is idempotent.
func (s *Source) Close() error {
	if !s.life.Close(nil) {
		return nil
	}
	s.closeTap()
	s.restoreOutput(context.Background())
	s.events.Close()
	return nil
}

// clockPair relates the host clock (Core Audio's mHostTime) to Go's
// monotonic clock. On macOS both count the same mach_absolute_time ticks,
// so one sample at start is enough: they never drift apart.
type clockPair struct {
	goTime time.Time
	hostNS uint64
}

// sampleClock takes the tightest of a few back-to-back readings.
func sampleClock() clockPair {
	best, span := clockPair{}, time.Duration(math.MaxInt64)
	for range 5 {
		t0 := time.Now()
		h := hostNowNS()
		d := time.Since(t0)
		if d < span {
			best, span = clockPair{goTime: t0.Add(d / 2), hostNS: h}, d
		}
	}
	return best
}

// at converts a host-clock reading to a Go monotonic time.
func (c clockPair) at(hostNS uint64) time.Time {
	return c.goTime.Add(time.Duration(int64(hostNS) - int64(c.hostNS)))
}

// osStatus renders an OSStatus as its four-character code when printable,
// else as a number.
func osStatus(st int32) string {
	u := uint32(st)
	b := []byte{byte(u >> 24), byte(u >> 16), byte(u >> 8), byte(u)}
	printable := true
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			printable = false
		}
	}
	if printable {
		return fmt.Sprintf("OSStatus '%s' (%d)", strings.TrimSpace(string(b)), st)
	}
	return fmt.Sprintf("OSStatus %d", st)
}
