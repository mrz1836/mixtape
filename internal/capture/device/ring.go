//go:build darwin && cgo

package device

import (
	"sync/atomic"
	"time"
)

// ring is a single-producer, single-consumer byte queue between the
// real-time audio callback (producer) and the drain goroutine (consumer).
// The producer never allocates, locks or blocks: when the consumer falls
// behind, a whole callback's frames are dropped and counted instead.
//
// Every write also records a stamp, the stream position where that write
// starts and the capture time of its first byte, so the consumer can put
// an accurate timestamp on each block it hands on.
//
// Publication order matters: the producer stores stampsW before written,
// and the consumer loads written before stampsW, so every stamp for bytes
// the consumer can see is visible to it.
type ring struct {
	buf  []byte
	size int64

	written atomic.Int64 // total bytes published by the producer
	read    atomic.Int64 // total bytes consumed

	stamps  []stamp
	stampsW atomic.Int64 // stamps published
	stampsR atomic.Int64 // first stamp still in use by the consumer

	overflows atomic.Int64 // writes dropped for lack of room
}

type stamp struct {
	pos int64 // stream byte position of the write
	at  time.Time
}

func newRing(bytes, stamps int) *ring {
	return &ring{buf: make([]byte, bytes), size: int64(bytes), stamps: make([]stamp, stamps)}
}

// write copies p into the ring with the capture time of its first byte.
// It reports false, and counts an overflow, when there is no room.
// Producer side only.
func (r *ring) write(p []byte, at time.Time) bool {
	n := int64(len(p))
	if n == 0 {
		return true
	}
	w, rd := r.written.Load(), r.read.Load()
	sw, sr := r.stampsW.Load(), r.stampsR.Load()
	if n > r.size-(w-rd) || sw-sr >= int64(len(r.stamps)) {
		r.overflows.Add(1)
		return false
	}
	off := w % r.size
	first := copy(r.buf[off:], p)
	copy(r.buf, p[first:])
	r.stamps[sw%int64(len(r.stamps))] = stamp{pos: w, at: at}
	r.stampsW.Store(sw + 1)
	r.written.Store(w + n)
	return true
}

// origin locates the first byte of a read in time: the stamp covering it
// and the byte's offset past that stamp's position.
type origin struct {
	at     time.Time
	offset int64 // bytes
	ok     bool  // false when no stamp covers the byte
}

// readInto moves up to len(dst) buffered bytes into dst and returns the
// byte count and the origin of the first byte, from which the caller
// computes its exact capture time. Consumer side only.
func (r *ring) readInto(dst []byte) (int, origin) {
	w := r.written.Load()
	sw := r.stampsW.Load()
	rd := r.read.Load()
	count := min(w-rd, int64(len(dst)))
	if count <= 0 {
		return 0, origin{}
	}

	var o origin
	sr := r.stampsR.Load()
	for sr+1 < sw && r.stampAt(sr+1).pos <= rd {
		sr++
	}
	if sr < sw {
		if s := r.stampAt(sr); s.pos <= rd {
			o = origin{at: s.at, offset: rd - s.pos, ok: true}
		}
	}

	off := rd % r.size
	first := copy(dst[:count], r.buf[off:])
	copy(dst[first:count], r.buf)
	r.read.Store(rd + count)

	// Release the stamps whose bytes are now entirely consumed, keeping
	// the one that covers the next read position.
	end := rd + count
	for sr+1 < sw && r.stampAt(sr+1).pos <= end {
		sr++
	}
	r.stampsR.Store(sr)
	return int(count), o
}

func (r *ring) stampAt(i int64) stamp { return r.stamps[i%int64(len(r.stamps))] }

// takeOverflows returns and resets the overflow count.
func (r *ring) takeOverflows() int64 { return r.overflows.Swap(0) }
