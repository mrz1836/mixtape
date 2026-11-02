//go:build darwin && cgo

// Package device captures audio from a Core Audio input device, typically
// BlackHole 2ch fed by a Multi-Output Device, through miniaudio (malgo).
//
// It records at the device's native sample rate, so nothing is resampled,
// and its real-time callback only copies into a lock-free ring; a drain
// goroutine hands the audio on.
package device

// #include <stdlib.h>
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gen2brain/malgo"

	"github.com/mrz1836/mixtape/internal/capture"
)

const (
	// channels is the channel count Mixtape records.
	channels = 2
	// ringSeconds of audio buffered between the callback and the drain.
	ringSeconds = 4
	// ringStamps is the number of callback timestamps buffered.
	ringStamps = 4096
)

var (
	// ErrNoDevice reports that no capture device matched the requested name.
	ErrNoDevice = errors.New("no capture device matching")
	// errNoDefault reports that Core Audio has no default output device.
	errNoDefault = errors.New("no default output device")
	// errFormat reports a device whose negotiated format is unusable.
	errFormat = errors.New("unusable capture format")
)

// Info describes one audio device.
type Info struct {
	ID            string
	Name          string
	Input         bool
	Output        bool
	DefaultInput  bool
	DefaultOutput bool
}

// List returns every capture and playback device. A device with both
// sides (BlackHole is one) appears once with Input and Output set.
func List() ([]Info, error) {
	mctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendCoreaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	defer func() { _ = mctx.Uninit(); mctx.Free() }()
	return list(mctx)
}

func list(mctx *malgo.AllocatedContext) ([]Info, error) {
	captures, err := mctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("enumerate capture devices: %w", err)
	}
	playbacks, err := mctx.Devices(malgo.Playback)
	if err != nil {
		return nil, fmt.Errorf("enumerate playback devices: %w", err)
	}

	var out []Info
	index := map[string]int{}
	add := func(d *malgo.DeviceInfo, input bool) {
		id := d.ID.String()
		i, seen := index[id]
		if !seen {
			out = append(out, Info{ID: id, Name: d.Name()})
			i = len(out) - 1
			index[id] = i
		}
		if input {
			out[i].Input = true
			out[i].DefaultInput = out[i].DefaultInput || d.IsDefault != 0
		} else {
			out[i].Output = true
			out[i].DefaultOutput = out[i].DefaultOutput || d.IsDefault != 0
		}
	}
	for i := range captures {
		add(&captures[i], true)
	}
	for i := range playbacks {
		add(&playbacks[i], false)
	}
	return out, nil
}

// DefaultOutput returns the device macOS currently plays through.
func DefaultOutput() (Info, error) {
	devices, err := List()
	if err != nil {
		return Info{}, err
	}
	for _, d := range devices {
		if d.DefaultOutput {
			return d, nil
		}
	}
	return Info{}, errNoDefault
}

// Source captures one input device. It implements capture.Source.
type Source struct {
	name   string
	format capture.Format

	mctx *malgo.AllocatedContext
	dev  *malgo.Device
	ring *ring

	life    capture.Lifecycle
	events  *capture.Emitter
	closing atomic.Bool // set before the device is stopped on purpose
}

var _ capture.Source = (*Source)(nil)

// Open prepares capture from the first input device whose name contains
// match (case-insensitive). It records 16-bit stereo at the device's
// native rate. Capture begins with Start.
func Open(match string) (*Source, error) {
	mctx, err := malgo.InitContext([]malgo.Backend{malgo.BackendCoreaudio}, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, fmt.Errorf("init audio context: %w", err)
	}
	s, err := open(mctx, match)
	if err != nil {
		_ = mctx.Uninit()
		mctx.Free()
		return nil, err
	}
	return s, nil
}

func open(mctx *malgo.AllocatedContext, match string) (*Source, error) {
	infos, err := mctx.Devices(malgo.Capture)
	if err != nil {
		return nil, fmt.Errorf("enumerate capture devices: %w", err)
	}
	var found *malgo.DeviceInfo
	names := make([]string, 0, len(infos))
	for i := range infos {
		name := infos[i].Name()
		if name != "" {
			names = append(names, name)
		}
		if found == nil && strings.Contains(strings.ToLower(name), strings.ToLower(match)) {
			found = &infos[i]
		}
	}
	if found == nil {
		available := "none"
		if len(names) > 0 {
			available = strings.Join(names, ", ")
		}
		return nil, fmt.Errorf("%w %q (available capture devices: %s)", ErrNoDevice, match, available)
	}

	s := &Source{name: found.Name(), mctx: mctx, events: capture.NewEmitter()}

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.SampleRate = 0 // the device's native rate: no resampling
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = channels
	// The device ID must live in C memory (cgo pointer rules); miniaudio
	// copies what it needs during init.
	idPtr := found.ID.Pointer()
	cfg.Capture.DeviceID = idPtr
	dev, err := malgo.InitDevice(mctx.Context, cfg, malgo.DeviceCallbacks{Data: s.onData, Stop: s.onStop})
	C.free(idPtr)
	if err != nil {
		return nil, fmt.Errorf("open capture device %q: %w", s.name, err)
	}

	rate, ch := dev.SampleRate(), dev.CaptureChannels()
	if rate == 0 || rate > math.MaxInt32 || ch == 0 || ch > math.MaxInt16 {
		dev.Uninit()
		return nil, fmt.Errorf("open capture device %q: %w: %d Hz, %d channels", s.name, errFormat, rate, ch)
	}
	s.dev = dev
	s.format = capture.Format{Rate: int(rate), Channels: int(ch)}
	s.ring = newRing(s.format.Rate*s.format.BytesPerFrame()*ringSeconds, ringStamps)
	return s, nil
}

// Name is the device name, e.g. "BlackHole 2ch".
func (s *Source) Name() string { return s.name }

// Method is capture.MethodDevice.
func (s *Source) Method() capture.Method { return capture.MethodDevice }

// Format is the native capture format.
func (s *Source) Format() capture.Format { return s.format }

// Events reports overflows and an unexpected stop.
func (s *Source) Events() <-chan capture.Event { return s.events.Events() }

// Start begins capture and delivers blocks to sink from a drain goroutine.
func (s *Source) Start(ctx context.Context, sink func(capture.Block)) error {
	begin := func() error {
		if err := s.dev.Start(); err != nil {
			return fmt.Errorf("start capture on %q: %w", s.name, err)
		}
		return nil
	}
	return s.life.Start(ctx, begin, func(dctx context.Context) { s.drain(dctx, sink) })
}

// onData runs on the real-time audio thread: it only copies into the ring.
func (s *Source) onData(_, input []byte, frames uint32) {
	at := time.Now().Add(-s.format.Duration(int64(frames)))
	s.ring.write(input, at)
}

// onStop runs when the device stops on its own (unplugged, reconfigured).
func (s *Source) onStop() {
	if !s.closing.Load() {
		s.events.Emit(capture.EventStopped, "the capture device stopped")
	}
}

// drain empties the ring every drainInterval and hands blocks to sink.
func (s *Source) drain(ctx context.Context, sink func(capture.Block)) {
	bpf := s.format.BytesPerFrame()
	buf := make([]byte, len(s.ring.buf))
	tick := time.NewTicker(capture.DrainInterval)
	defer tick.Stop()
	for {
		for {
			n, o := s.ring.readInto(buf)
			if n == 0 {
				break
			}
			var at time.Time
			if o.ok {
				at = o.at.Add(s.format.Duration(o.offset / int64(bpf)))
			}
			sink(capture.Block{PCM: buf[:n], Frames: n / bpf, At: at})
		}
		if dropped := s.ring.takeOverflows(); dropped > 0 {
			s.events.Emit(capture.EventOverflow, fmt.Sprintf("%d audio callbacks dropped: the reader fell behind", dropped))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// Close stops capture and frees the device and the audio context. It is
// idempotent.
func (s *Source) Close() error {
	halt := func() {
		s.closing.Store(true)
		s.dev.Uninit() // stops the device; no callback runs after it returns
	}
	if !s.life.Close(halt) {
		return nil
	}
	err := s.mctx.Uninit()
	s.mctx.Free()
	s.events.Close()
	return err
}
