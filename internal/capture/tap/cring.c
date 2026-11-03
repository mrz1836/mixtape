//go:build darwin && cgo

#include "cring.h"

#include <mach/mach_time.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

int mt_ring_init(mt_ring *r, uint64_t capacity, uint32_t channels) {
    memset(r, 0, sizeof(*r));
    if (capacity == 0 || channels == 0) {
        return -1;
    }
    r->buf = calloc((size_t)(capacity * channels), sizeof(float));
    if (r->buf == NULL) {
        return -1;
    }
    r->capacity = capacity;
    r->channels = channels;
    return 0;
}

void mt_ring_free(mt_ring *r) {
    free(r->buf);
    r->buf = NULL;
    r->capacity = 0;
}

// reserve returns the stream position for frames, or UINT64_MAX on overflow.
static uint64_t reserve(mt_ring *r, uint32_t frames) {
    uint64_t w = atomic_load_explicit(&r->written, memory_order_relaxed);
    uint64_t rd = atomic_load_explicit(&r->read, memory_order_acquire);
    if ((uint64_t)frames > r->capacity - (w - rd)) {
        atomic_fetch_add_explicit(&r->overflows, 1, memory_order_relaxed);
        return UINT64_MAX;
    }
    return w;
}

static void publish(mt_ring *r, uint64_t w, uint32_t frames, uint64_t host_ns) {
    uint32_t s = atomic_load_explicit(&r->seq, memory_order_relaxed);
    atomic_store_explicit(&r->seq, s + 1, memory_order_relaxed);
    atomic_thread_fence(memory_order_release);
    atomic_store_explicit(&r->anchor_frame, w, memory_order_relaxed);
    atomic_store_explicit(&r->anchor_host_ns, host_ns, memory_order_relaxed);
    atomic_store_explicit(&r->seq, s + 2, memory_order_release);
    atomic_store_explicit(&r->written, w + frames, memory_order_release);
}

// source_channel is the source channel that ring channel c takes: mono is
// duplicated, extra channels are dropped.
static inline uint32_t source_channel(uint32_t c, uint32_t src_channels) {
    return c < src_channels ? c : src_channels - 1;
}

int mt_ring_write_interleaved(mt_ring *r, const float *src, uint32_t frames,
                              uint32_t src_channels, uint64_t host_ns) {
    if (frames == 0 || src_channels == 0) {
        return 1;
    }
    uint64_t w = reserve(r, frames);
    if (w == UINT64_MAX) {
        return 0;
    }
    for (uint32_t f = 0; f < frames; f++) {
        float *dst = r->buf + ((w + f) % r->capacity) * r->channels;
        const float *frame = src + (size_t)f * src_channels;
        for (uint32_t c = 0; c < r->channels; c++) {
            dst[c] = frame[source_channel(c, src_channels)];
        }
    }
    publish(r, w, frames, host_ns);
    return 1;
}

int mt_ring_write_planar(mt_ring *r, const float *const *planes, uint32_t frames,
                         uint32_t src_channels, uint64_t host_ns) {
    if (frames == 0 || src_channels == 0) {
        return 1;
    }
    uint64_t w = reserve(r, frames);
    if (w == UINT64_MAX) {
        return 0;
    }
    for (uint32_t f = 0; f < frames; f++) {
        float *dst = r->buf + ((w + f) % r->capacity) * r->channels;
        for (uint32_t c = 0; c < r->channels; c++) {
            dst[c] = planes[source_channel(c, src_channels)][f];
        }
    }
    publish(r, w, frames, host_ns);
    return 1;
}

uint64_t mt_ring_read(mt_ring *r, float *dst, uint64_t max_frames, uint64_t *first_frame) {
    uint64_t w = atomic_load_explicit(&r->written, memory_order_acquire);
    uint64_t rd = atomic_load_explicit(&r->read, memory_order_relaxed);
    uint64_t n = w - rd;
    if (n > max_frames) {
        n = max_frames;
    }
    *first_frame = rd;
    for (uint64_t done = 0; done < n;) {
        uint64_t off = (rd + done) % r->capacity;
        uint64_t run = r->capacity - off;
        if (run > n - done) {
            run = n - done;
        }
        memcpy(dst + done * r->channels, r->buf + off * r->channels,
               (size_t)(run * r->channels) * sizeof(float));
        done += run;
    }
    atomic_store_explicit(&r->read, rd + n, memory_order_release);
    return n;
}

int mt_ring_anchor(mt_ring *r, uint64_t *frame, uint64_t *host_ns) {
    for (;;) {
        uint32_t s1 = atomic_load_explicit(&r->seq, memory_order_acquire);
        if (s1 & 1) {
            continue; // a write is in progress
        }
        uint64_t f = atomic_load_explicit(&r->anchor_frame, memory_order_relaxed);
        uint64_t h = atomic_load_explicit(&r->anchor_host_ns, memory_order_relaxed);
        atomic_thread_fence(memory_order_acquire);
        uint32_t s2 = atomic_load_explicit(&r->seq, memory_order_relaxed);
        if (s1 == s2) {
            if (s1 == 0) {
                return 0;
            }
            *frame = f;
            *host_ns = h;
            return 1;
        }
    }
}

uint64_t mt_ring_take_overflows(mt_ring *r) {
    return atomic_exchange_explicit(&r->overflows, 0, memory_order_relaxed);
}

uint64_t mt_host_now_ns(void) {
    return clock_gettime_nsec_np(CLOCK_UPTIME_RAW);
}

uint64_t mt_host_ticks_to_ns(uint64_t ticks) {
    static mach_timebase_info_data_t tb;
    if (tb.denom == 0) {
        mach_timebase_info(&tb);
    }
    return (uint64_t)(((__uint128_t)ticks * tb.numer) / tb.denom);
}
