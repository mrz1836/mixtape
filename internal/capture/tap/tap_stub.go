//go:build !darwin || !cgo

// Package tap records Spotify's audio with a Core Audio process tap. This
// build has no Core Audio (not macOS, or cgo disabled): the tap is never
// available and Open always fails.
package tap

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/mrz1836/mixtape/internal/capture"
)

var (
	// ErrNoProcess reports that Spotify has no audio process to tap.
	ErrNoProcess = errors.New("spotify has no audio process to tap (is it running?)")
	// ErrCreateTap reports a Core Audio failure while building the tap.
	ErrCreateTap = errors.New("could not create the process tap")
)

const unsupportedReason = "process taps need macOS 14.2+ and a cgo build"

// Available always reports the tap as unsupported in this build.
func Available() capture.TapStatus { return capture.TapStatus{Reason: unsupportedReason} }

// Process is one audio process object.
type Process struct {
	ObjectID uint32
	PID      int
	BundleID string
}

// SpotifyProcesses is unsupported in this build.
func SpotifyProcesses() ([]Process, error) { return nil, capture.ErrTapUnsupported }

// Options configures Open.
type Options struct {
	Mute         bool
	BundlePrefix string
	Wait         time.Duration
}

// Source is unavailable in this build.
type Source struct{}

var _ capture.Source = (*Source)(nil)

// Open is unsupported in this build.
func Open(context.Context, Options) (*Source, error) {
	return nil, fmt.Errorf("%w: %s", capture.ErrTapUnsupported, unsupportedReason)
}

// Name is empty in this build.
func (*Source) Name() string { return "" }

// Method is capture.MethodTap.
func (*Source) Method() capture.Method { return capture.MethodTap }

// Format is zero in this build.
func (*Source) Format() capture.Format { return capture.Format{} }

// Start is unsupported in this build.
func (*Source) Start(context.Context, func(capture.Block)) error { return capture.ErrTapUnsupported }

// Events is nil in this build.
func (*Source) Events() <-chan capture.Event { return nil }

// Close is a no-op in this build.
func (*Source) Close() error { return nil }
