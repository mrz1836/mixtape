//go:build darwin && cgo

package tap

/*
#include <stdlib.h>
#include "cring.h"
*/
import "C"

import (
	"unsafe"
)

// cRing wraps a C mt_ring allocated in C memory (it holds C11 atomics that
// Go must never copy or move). It exists so the real-time copying and
// queueing code can be unit-tested from Go; the tap itself drives the ring
// from Objective-C.
type cRing struct{ r *C.mt_ring }

func newCRing(capacity uint64, channels uint32) (*cRing, bool) {
	r := (*C.mt_ring)(C.calloc(1, C.sizeof_mt_ring))
	if r == nil {
		return nil, false
	}
	if C.mt_ring_init(r, C.uint64_t(capacity), C.uint32_t(channels)) != 0 {
		C.free(unsafe.Pointer(r))
		return nil, false
	}
	return &cRing{r: r}, true
}

func (c *cRing) free() {
	C.mt_ring_free(c.r)
	C.free(unsafe.Pointer(c.r))
	c.r = nil
}

// writeInterleaved copies src (frames × srcChannels samples) into C memory
// and writes it, as the IO block would.
func (c *cRing) writeInterleaved(src []float32, srcChannels uint32, hostNS uint64) bool {
	if len(src) == 0 || srcChannels == 0 {
		return C.mt_ring_write_interleaved(c.r, nil, 0, C.uint32_t(srcChannels), C.uint64_t(hostNS)) == 1
	}
	buf := cFloats(src)
	defer C.free(unsafe.Pointer(buf))
	frames := uint32(uint64(len(src)) / uint64(srcChannels))
	return C.mt_ring_write_interleaved(c.r, buf, C.uint32_t(frames), C.uint32_t(srcChannels), C.uint64_t(hostNS)) == 1
}

// writePlanar writes one slice per channel, all the same length.
func (c *cRing) writePlanar(planes [][]float32, hostNS uint64) bool {
	if len(planes) == 0 {
		return true
	}
	base := (**C.float)(C.calloc(C.size_t(len(planes)), C.size_t(unsafe.Sizeof((*C.float)(nil)))))
	defer C.free(unsafe.Pointer(base))
	ptrs := unsafe.Slice(base, len(planes))
	for i, p := range planes {
		ptrs[i] = cFloats(p)
	}
	defer func() {
		for _, p := range ptrs {
			C.free(unsafe.Pointer(p))
		}
	}()
	frames := uint32(len(planes[0]))
	channels := uint32(len(planes))
	return C.mt_ring_write_planar(c.r, base, C.uint32_t(frames), C.uint32_t(channels), C.uint64_t(hostNS)) == 1
}

// read returns up to maxFrames frames and the stream position of the first.
func (c *cRing) read(maxFrames uint64) ([]float32, uint64) {
	channels := uint64(c.r.channels)
	if maxFrames == 0 || channels == 0 {
		return nil, 0
	}
	out := make([]float32, maxFrames*channels)
	n, first := readRing(c.r, out, maxFrames)
	return out[:n*channels], first
}

// anchor returns the latest (frame, host ns) anchor.
func (c *cRing) anchor() (frame, hostNS uint64, ok bool) {
	var f, h C.uint64_t
	if C.mt_ring_anchor(c.r, &f, &h) == 0 {
		return 0, 0, false
	}
	return uint64(f), uint64(h), true
}

func (c *cRing) takeOverflows() uint64 { return uint64(C.mt_ring_take_overflows(c.r)) }

// hostNowNS is the host clock in nanoseconds (CLOCK_UPTIME_RAW).
func hostNowNS() uint64 { return uint64(C.mt_host_now_ns()) }

// readRing moves up to maxFrames frames of r into dst, which must hold that
// many, and returns the count and the stream position of the first.
func readRing(r *C.mt_ring, dst []float32, maxFrames uint64) (n, first uint64) {
	var f C.uint64_t
	got := C.mt_ring_read(r, (*C.float)(unsafe.Pointer(&dst[0])), C.uint64_t(maxFrames), &f)
	return uint64(got), uint64(f)
}

// cFloats copies s into newly C-allocated memory; the caller frees it.
func cFloats(s []float32) *C.float {
	p := (*C.float)(C.malloc(C.size_t(len(s)) * C.size_t(unsafe.Sizeof(C.float(0)))))
	copy(unsafe.Slice((*float32)(unsafe.Pointer(p)), len(s)), s)
	return p
}
