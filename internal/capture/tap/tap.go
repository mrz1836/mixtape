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
)

const (
	// DefaultBundlePrefix matches Spotify's app and its helper processes.
	DefaultBundlePrefix = "com.spotify.client"

	ringSeconds    = 4.0
	drainInterval  = 10 * time.Millisecond
	waitInterval   = 250 * time.Millisecond
	retryInterval  = time.Second
	eventBuffer    = 16
	maxProcesses   = 64
	maxBundleBytes = 512
	channels       = 2
)

var (
	// ErrNoProcess reports that Spotify has no audio process to tap.
	ErrNoProcess = errors.New("spotify has no audio process to tap (is it running?)")
	// ErrCreateTap reports a Core Audio failure while building the tap.
	ErrCreateTap = errors.New("could not create the process tap")
	// ErrStarted reports a second Start on the same Source.
	ErrStarted = errors.New("capture already started")
	// ErrClosed reports use of a closed Source.
	ErrClosed = errors.New("capture source closed")
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
func SpotifyProcesses() ([]Process, error) { return processes(DefaultBundlePrefix) }

func processes(prefix string) ([]Process, error) {
	if !Available().Supported {
		return nil, capture.ErrTapUnsupported
	}
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
	// BundlePrefix selects the processes to tap; empty means Spotify.
	BundlePrefix string
	// Wait is how long Open waits for the processes to appear.
	Wait time.Duration
}

// Source records the tapped processes. It implements capture.Source.
type Source struct {
	opts Options

	mu      sync.Mutex
	tap     unsafe.Pointer // retained MTTap, owned by the drain once started
	procs   []uint32
	format  capture.Format
	started bool
	closed  bool
	cancel  context.CancelFunc
	done    chan struct{}

	events   chan capture.Event
	evMu     sync.Mutex
	evClosed bool
}

var _ capture.Source = (*Source)(nil)

// Open builds the tap on Spotify's audio processes, waiting up to o.Wait
// for them to appear. Audio flows to the sink once Start is called.
func Open(o Options) (*Source, error) {
	if st := Available(); !st.Supported {
		return nil, fmt.Errorf("%w: %s", capture.ErrTapUnsupported, st.Reason)
	}
	if o.BundlePrefix == "" {
		o.BundlePrefix = DefaultBundlePrefix
	}
	procs, err := waitForProcesses(o.BundlePrefix, o.Wait)
	if err != nil {
		return nil, err
	}
	s := &Source{opts: o, events: make(chan capture.Event, eventBuffer)}
	if err := s.build(procs); err != nil {
		return nil, err
	}
	return s, nil
}

func waitForProcesses(prefix string, wait time.Duration) ([]uint32, error) {
	deadline := time.Now().Add(wait)
	for {
		procs, err := processes(prefix)
		if err != nil {
			return nil, err
		}
		if len(procs) > 0 {
			ids := make([]uint32, 0, len(procs))
			for _, p := range procs {
				ids = append(ids, p.ObjectID)
			}
			slices.Sort(ids)
			return ids, nil
		}
		if !time.Now().Before(deadline) {
			return nil, ErrNoProcess
		}
		time.Sleep(waitInterval)
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
	s.format = capture.Format{Rate: int(math.Round(rate)), Channels: channels}
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
func (s *Source) Events() <-chan capture.Event { return s.events }

// Start delivers blocks to sink from a drain goroutine.
func (s *Source) Start(ctx context.Context, sink func(capture.Block)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.closed:
		return ErrClosed
	case s.started:
		return ErrStarted
	}
	s.started = true
	dctx, cancel := context.WithCancel(ctx)
	s.cancel, s.done = cancel, make(chan struct{})
	go s.drain(dctx, sink)
	return nil
}

// drain runs until ctx ends: it reads the ring, stamps blocks, answers
// Core Audio's change notifications by rebuilding the tap, and reports
// overflows.
func (s *Source) drain(ctx context.Context, sink func(capture.Block)) {
	defer close(s.done)
	clock := sampleClock()
	tick := time.NewTicker(drainInterval)
	defer tick.Stop()
	var buf []byte
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
			buf = s.read(t, format, clock, buf, sink)
			s.handleEvents(t)
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// read drains the ring of tap t into sink.
func (s *Source) read(t unsafe.Pointer, f capture.Format, clock clockPair, buf []byte, sink func(capture.Block)) []byte {
	ring := C.mt_tap_ring(t)
	maxFrames := uint64(ring.capacity)
	need := int(maxFrames) * f.BytesPerFrame()
	if cap(buf) < need {
		buf = make([]byte, need)
	}
	buf = buf[:need]
	for {
		var first C.uint64_t
		n := uint64(C.mt_ring_read(ring, (*C.int16_t)(unsafe.Pointer(&buf[0])), C.uint64_t(maxFrames), &first))
		if n == 0 {
			break
		}
		var at time.Time
		var af, ah C.uint64_t
		if C.mt_ring_anchor(ring, &af, &ah) == 1 {
			at = clock.at(uint64(ah)).Add(-f.Duration(int64(uint64(af)) - int64(uint64(first))))
		}
		sink(capture.Block{PCM: buf[:int(n)*f.BytesPerFrame()], Frames: int(n), At: at})
	}
	if n := uint64(C.mt_ring_take_overflows(ring)); n > 0 {
		s.emit(capture.EventOverflow, fmt.Sprintf("%d tap IO cycles dropped: the reader fell behind", n))
	}
	return buf
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
		procs, err := waitForProcesses(s.opts.BundlePrefix, 0)
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
	procs, err := waitForProcesses(s.opts.BundlePrefix, 0)
	if err == nil {
		err = s.build(procs)
	}
	if err != nil {
		if old != nil {
			s.emit(kind, detail+"; capture paused: "+err.Error())
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
	s.emit(kind, detail)
}

// emit sends an event without blocking; it is a no-op after Close.
func (s *Source) emit(kind capture.EventKind, detail string) {
	s.evMu.Lock()
	defer s.evMu.Unlock()
	if s.evClosed {
		return
	}
	select {
	case s.events <- capture.Event{Kind: kind, Detail: detail, At: time.Now()}:
	default:
	}
}

// Close stops capture and destroys the tap. It is idempotent.
func (s *Source) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cancel, done := s.cancel, s.done
	s.mu.Unlock()

	if cancel != nil {
		cancel()
		<-done
	}
	s.mu.Lock()
	if s.tap != nil {
		C.mt_tap_close(s.tap)
		s.tap = nil
	}
	s.mu.Unlock()

	s.evMu.Lock()
	s.evClosed = true
	close(s.events)
	s.evMu.Unlock()
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
