//go:build !darwin || !cgo

// Package device captures audio from a Core Audio input device. This build
// has no Core Audio (not macOS, or cgo disabled), so every call fails with
// errors.ErrUnsupported.
package device

import (
	"context"
	"errors"
	"fmt"

	"github.com/mrz1836/mixtape/internal/capture"
)

// ErrNoDevice reports that no capture device matched the requested name.
var ErrNoDevice = errors.New("no capture device matching")

var errNoCoreAudio = fmt.Errorf("capture device: %w (needs macOS and cgo)", errors.ErrUnsupported)

// Info describes one audio device.
type Info struct {
	ID            string
	Name          string
	Input         bool
	Output        bool
	DefaultInput  bool
	DefaultOutput bool
}

// List is unsupported in this build.
func List() ([]Info, error) { return nil, errNoCoreAudio }

// DefaultOutput is unsupported in this build.
func DefaultOutput() (Info, error) { return Info{}, errNoCoreAudio }

// Source is unavailable in this build.
type Source struct{}

var _ capture.Source = (*Source)(nil)

// Open is unsupported in this build.
func Open(string) (*Source, error) { return nil, errNoCoreAudio }

// Name is empty in this build.
func (*Source) Name() string { return "" }

// Method is capture.MethodDevice.
func (*Source) Method() capture.Method { return capture.MethodDevice }

// Format is zero in this build.
func (*Source) Format() capture.Format { return capture.Format{} }

// Start is unsupported in this build.
func (*Source) Start(context.Context, func(capture.Block)) error { return errNoCoreAudio }

// Events is nil in this build.
func (*Source) Events() <-chan capture.Event { return nil }

// Close is a no-op in this build.
func (*Source) Close() error { return nil }
