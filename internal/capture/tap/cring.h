// cring.h — a lock-free single-producer, single-consumer ring of Float32
// PCM frames, written by a Core Audio IO block and read by a Go goroutine.
//
// The producer (the real-time IO thread) never locks, allocates or calls
// into Go: it copies the Float32 samples in as they are, mapping channels,
// and publishes the stream position and host time of the write as the
// "latest anchor" under a seqlock. When the consumer falls behind, whole
// writes are dropped and counted instead of blocking. Conversion to integer
// PCM, at whatever depth the session records, happens in Go.

#ifndef MIXTAPE_CRING_H
#define MIXTAPE_CRING_H

#include <stdatomic.h>
#include <stdint.h>

typedef struct mt_ring {
    float *buf;
    uint64_t capacity;  // frames
    uint32_t channels;  // output channels (interleaved)

    _Atomic uint64_t written;   // frames published by the producer
    _Atomic uint64_t read;      // frames consumed
    _Atomic uint64_t overflows; // writes dropped for lack of room

    // Latest anchor, guarded by a seqlock: odd seq means a write is in progress.
    _Atomic uint32_t seq;
    _Atomic uint64_t anchor_frame;
    _Atomic uint64_t anchor_host_ns;
} mt_ring;

// mt_ring_init allocates room for capacity frames of channels samples.
// It returns 0 on success, -1 when out of memory or given zero sizes.
int mt_ring_init(mt_ring *r, uint64_t capacity, uint32_t channels);

// mt_ring_free releases the buffer. The ring must not be in use.
void mt_ring_free(mt_ring *r);

// mt_ring_write_interleaved copies frames of interleaved Float32 audio with
// src_channels channels into the ring's channel count (extra channels are
// dropped, mono is duplicated) and publishes them with host_ns as the
// capture time of the first frame. It returns 1 when written, 0 on overflow.
int mt_ring_write_interleaved(mt_ring *r, const float *src, uint32_t frames,
                              uint32_t src_channels, uint64_t host_ns);

// mt_ring_write_planar is mt_ring_write_interleaved for non-interleaved
// audio: planes[c] holds channel c.
int mt_ring_write_planar(mt_ring *r, const float *const *planes, uint32_t frames,
                         uint32_t src_channels, uint64_t host_ns);

// mt_ring_read moves up to max_frames frames into dst and returns the count.
// *first_frame receives the stream position of the first frame read.
uint64_t mt_ring_read(mt_ring *r, float *dst, uint64_t max_frames, uint64_t *first_frame);

// mt_ring_anchor reads the latest anchor. It returns 0 when no write has
// happened yet.
int mt_ring_anchor(mt_ring *r, uint64_t *frame, uint64_t *host_ns);

// mt_ring_take_overflows returns and resets the overflow count.
uint64_t mt_ring_take_overflows(mt_ring *r);

// mt_host_now_ns is the host clock (mach_absolute_time) in nanoseconds:
// the clock Core Audio's mHostTime uses.
uint64_t mt_host_now_ns(void);

// mt_host_ticks_to_ns converts mach_absolute_time ticks to nanoseconds.
uint64_t mt_host_ticks_to_ns(uint64_t ticks);

#endif
