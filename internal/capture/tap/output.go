//go:build darwin && cgo

package tap

/*
#include <stdlib.h>
#include "coutput.h"
*/
import "C"

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"

	"github.com/mrz1836/mixtape/internal/capture"
)

// Timing of an output rate switch, from measurements on built-in speakers
// (macOS 15): the set returns at once, the rate reads back new after about
// 55 ms and the device announces it after about 100 ms; with a call app
// holding the device (Microsoft Teams), the app set it back after about
// 220 ms. The settle outlasts that, so a switch that will not hold is
// caught before the tap is built on it.
const (
	listenerWait  = 2 * time.Second
	readBackWait  = 2 * time.Second
	readBackPoll  = 10 * time.Millisecond
	settleTime    = 500 * time.Millisecond
	maxRateRanges = 32
	uidBytes      = 512
)

// errOutputControl reports a Core Audio failure reading or setting a rate.
var errOutputControl = errors.New("core audio")

// OutputControl reads and sets output device rates through Core Audio.
func OutputControl() capture.OutputControl { return coreAudioOutput{} }

type coreAudioOutput struct{}

var _ capture.OutputControl = coreAudioOutput{}

// DefaultOutput describes the device macOS plays through.
func (coreAudioOutput) DefaultOutput() (capture.OutputDevice, error) {
	uid := (*C.char)(C.malloc(uidBytes))
	defer C.free(unsafe.Pointer(uid))
	name := (*C.char)(C.malloc(uidBytes))
	defer C.free(unsafe.Pointer(name))
	var (
		rate   C.double
		ranges [2 * maxRateRanges]C.double
		n      C.int
	)
	st := C.mt_output_default(uid, uidBytes, name, uidBytes, &rate, &ranges[0], maxRateRanges, &n)
	if err := outputErr(int32(st), "read the default output"); err != nil {
		return capture.OutputDevice{}, err
	}
	d := capture.OutputDevice{UID: C.GoString(uid), Name: C.GoString(name), Rate: hz(float64(rate))}
	for i := range int(n) {
		d.Rates = append(d.Rates, capture.RateRange{Min: float64(ranges[2*i]), Max: float64(ranges[2*i+1])})
	}
	return d, nil
}

// OutputRate is the nominal rate of the device with uid.
func (coreAudioOutput) OutputRate(uid string) (int, error) {
	cuid := C.CString(uid)
	defer C.free(unsafe.Pointer(cuid))
	var rate C.double
	if err := outputErr(int32(C.mt_output_rate(cuid, &rate)), "read the output rate"); err != nil {
		return 0, err
	}
	return hz(float64(rate)), nil
}

// SetOutputRate sets the rate, waits for the device to announce and report
// it, then lets it settle and checks that it held.
func (c coreAudioOutput) SetOutputRate(ctx context.Context, uid string, rate int) error {
	cuid := C.CString(uid)
	defer C.free(unsafe.Pointer(cuid))
	var notified C.int
	st := C.mt_output_set_rate(cuid, C.double(rate), C.int(listenerWait.Milliseconds()), &notified)
	if err := outputErr(int32(st), "set the output rate"); err != nil {
		return err
	}
	deadline := time.Now().Add(readBackWait)
	for {
		now, err := c.OutputRate(uid)
		if err != nil {
			return err
		}
		if now == rate {
			break
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%w: still at %d Hz after %s", capture.ErrOutputTimeout, now, readBackWait)
		}
		if !sleep(ctx, readBackPoll) {
			return ctx.Err()
		}
	}
	if !sleep(ctx, settleTime) {
		return ctx.Err()
	}
	now, err := c.OutputRate(uid)
	if err != nil {
		return err
	}
	if now != rate {
		return fmt.Errorf("%w (to %d Hz)", capture.ErrOutputReverted, now)
	}
	return nil
}

// outputErr maps a Core Audio status to an error, nil for noErr.
func outputErr(st int32, what string) error {
	switch st {
	case 0:
		return nil
	case int32(C.MT_NO_DEVICE):
		return capture.ErrOutputGone
	}
	return fmt.Errorf("%w: %s: %s", errOutputControl, what, osStatus(st))
}

// hz rounds a nominal rate to whole hertz.
func hz(rate float64) int { return int(math.Round(rate)) }

// sleep waits for d or the end of ctx, and reports whether d passed.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
