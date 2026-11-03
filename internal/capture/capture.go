// Package capture defines how Mixtape receives audio, independent of the
// backend. It holds only plain types and the method selection logic, with
// no cgo, so the engine and its tests never link Core Audio.
//
// Two backends implement Source:
//
//   - capture/tap: a Core Audio process tap on Spotify's audio (macOS 14.2+).
//     Nothing to install, records only Spotify, can record muted.
//   - capture/device: a capture device such as BlackHole 2ch, fed by a
//     Multi-Output Device. Works on any macOS and over ssh.
//
// Audio is always interleaved signed little-endian PCM, 16 or 24 bits per
// sample (24-bit samples packed in three bytes), at the backend's native
// rate: nothing is resampled. Both backends receive Float32 from Core
// Audio and convert it with ConvertF32.
package capture

import (
	"context"
	"errors"
	"time"
)

// Method is a capture backend choice.
type Method string

// Capture methods.
const (
	MethodAuto   Method = "auto"
	MethodTap    Method = "tap"
	MethodDevice Method = "device"
)

// Format describes captured PCM: interleaved, signed, little-endian.
// Formats compare as they are: a Bits of 0 reads as 16 (SampleBits), but is
// not the same Format as 16.
type Format struct {
	Rate     int // frames per second
	Channels int
	Bits     int // bits per sample: 16 or 24; 0 means 16
}

// Sample widths a Format may have.
const (
	Bits16 = 16
	Bits24 = 24
)

// SampleBits is the bits per sample: Bits, or 16 when it is 0. Everything
// that sizes or reads samples goes through it.
func (f Format) SampleBits() int {
	if f.Bits == 0 {
		return Bits16
	}
	return f.Bits
}

// BytesPerSample is the size of one sample: 2 or 3 bytes.
func (f Format) BytesPerSample() int { return f.SampleBits() / 8 }

// BytesPerFrame is the size of one frame (one sample per channel).
func (f Format) BytesPerFrame() int { return f.Channels * f.BytesPerSample() }

// Valid reports a usable format: a positive rate and channel count, and
// 0, 16 or 24 bits.
func (f Format) Valid() bool {
	return f.Rate > 0 && f.Channels > 0 && (f.Bits == 0 || f.Bits == Bits16 || f.Bits == Bits24)
}

// Duration converts a frame count to a duration at this rate.
func (f Format) Duration(frames int64) time.Duration {
	if f.Rate <= 0 {
		return 0
	}
	return time.Duration(float64(frames) / float64(f.Rate) * float64(time.Second))
}

// Frames converts a duration to a frame count at this rate.
func (f Format) Frames(d time.Duration) int64 {
	return int64(d.Seconds() * float64(f.Rate))
}

// Block is a run of captured frames.
type Block struct {
	// PCM holds Frames frames in the source's Format. It is only valid
	// for the duration of the sink call: the receiver must copy it.
	PCM    []byte
	Frames int
	// At is when the first frame was captured, on the monotonic clock.
	At time.Time
}

// EventKind classifies a capture event.
type EventKind string

// Capture events. Any of them during a song means its audio cannot be
// trusted, so the engine marks that song failed.
const (
	// EventFormatChanged: the sample rate or channel count changed.
	EventFormatChanged EventKind = "format_changed"
	// EventDeviceChanged: the default output or capture device changed.
	EventDeviceChanged EventKind = "device_changed"
	// EventProcessChanged: Spotify's audio processes changed (relaunch)
	// and the tap was rebuilt.
	EventProcessChanged EventKind = "process_changed"
	// EventOverflow: frames were dropped because the reader fell behind.
	EventOverflow EventKind = "overflow"
	// EventStopped: the backend stopped delivering audio for good.
	EventStopped EventKind = "stopped"
)

// Event reports something that happened to the capture stream.
type Event struct {
	Kind   EventKind
	Detail string
	At     time.Time
}

// Source is a running or startable audio capture.
type Source interface {
	// Name describes the source for people, e.g. "process tap (Spotify)"
	// or "BlackHole 2ch".
	Name() string
	// Method is the backend.
	Method() Method
	// Format is the current PCM format. It can change after an
	// EventFormatChanged.
	Format() Format
	// Start begins delivering blocks to sink on a goroutine the source
	// owns (never the real-time audio thread). It returns once capture is
	// running. sink must not block for long.
	Start(ctx context.Context, sink func(Block)) error
	// Events reports format, device and overflow events. The channel is
	// closed by Close.
	Events() <-chan Event
	// Close stops capture and releases every resource. It is idempotent.
	Close() error
}

// TapStatus reports whether the process tap can be used on this Mac. It is
// the "tap" object of `mixtape devices -o json`.
type TapStatus struct {
	Supported bool   `json:"supported"`        // macOS 14.2 or later, built with cgo
	Reason    string `json:"reason,omitempty"` // why not, when unsupported
}

// Errors from method selection.
var (
	// ErrUnknownMethod reports a method name other than auto, tap or device.
	ErrUnknownMethod = errors.New("unknown capture method")
	// ErrTapUnsupported reports a request for the tap where it cannot run.
	ErrTapUnsupported = errors.New("the process tap is not available on this Mac")
)
