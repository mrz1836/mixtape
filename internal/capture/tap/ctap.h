// ctap.h — the Core Audio process tap behind package tap.
//
// mt_tap_open builds a private process tap on the given audio process
// objects, wraps it in a private aggregate device clocked by the default
// output, and starts an IO block that feeds an mt_ring. Property listeners
// raise event bits that Go polls with mt_tap_take_events and answers by
// rebuilding the tap. Every 14.2 API sits behind @available, so the binary
// still loads on older macOS (where mt_tap_supported returns 0).

#ifndef MIXTAPE_CTAP_H
#define MIXTAPE_CTAP_H

#include <CoreAudio/CoreAudio.h>
#include <stdint.h>

#include "cring.h"

// Event bits returned by mt_tap_take_events.
enum {
    MT_EVENT_PROCESSES = 1, // the system's audio process list changed
    MT_EVENT_OUTPUT = 2,    // the default output device changed
    MT_EVENT_RATE = 4,      // the aggregate's nominal sample rate changed
};

// mt_tap_supported reports whether process taps exist on this macOS.
int mt_tap_supported(void);

// mt_list_processes fills ids/pids with up to max audio process objects
// whose bundle ID starts with prefix and stores the count found in *count.
OSStatus mt_list_processes(const char *prefix, AudioObjectID *ids, int32_t *pids, int max, int *count);

// mt_process_bundle copies the bundle ID of process object id into buf.
OSStatus mt_process_bundle(AudioObjectID id, char *buf, int len);

// mt_tap_open creates and starts a tap. On failure it returns NULL and sets
// *status and *stage (a static string naming the failed step).
void *mt_tap_open(const AudioObjectID *ids, int count, int mute, double ring_seconds,
                  OSStatus *status, const char **stage);

// mt_tap_ring returns the ring the IO block writes.
mt_ring *mt_tap_ring(void *tap);

// mt_tap_rate and mt_tap_channels report the captured format.
double mt_tap_rate(void *tap);
uint32_t mt_tap_channels(void *tap);

// mt_tap_take_events returns and clears the pending MT_EVENT_* bits.
uint32_t mt_tap_take_events(void *tap);

// mt_tap_close stops IO and destroys the aggregate and the tap.
void mt_tap_close(void *tap);

#endif
