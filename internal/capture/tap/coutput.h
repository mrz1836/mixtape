// coutput.h — reading and setting the nominal sample rate of output
// devices, for the output rate a session may run the speakers at.
//
// Devices are named by UID, which stays the same across reboots and
// reconnects, so a rate can be put back on the right device whatever the
// default output is by then.

#ifndef MIXTAPE_COUTPUT_H
#define MIXTAPE_COUTPUT_H

#include <CoreAudio/CoreAudio.h>
#include <stdint.h>

// MT_NO_DEVICE is the status for a UID no connected device has.
#define MT_NO_DEVICE ((OSStatus)'nodv')

// mt_output_default describes the default output device: its UID and name
// (as UTF-8, truncated to fit), nominal rate, and the rates it offers as
// up to max_ranges (minimum, maximum) pairs in ranges, counted in
// *n_ranges.
OSStatus mt_output_default(char *uid, int uid_len, char *name, int name_len, double *rate, double *ranges,
                           int max_ranges, int *n_ranges);

// mt_output_rate reads the nominal rate of the device with uid.
OSStatus mt_output_rate(const char *uid, double *rate);

// mt_output_set_rate sets the nominal rate of the device with uid and waits
// up to timeout_ms for the device to announce the change. *notified is 1
// when it did; a device already at rate announces nothing.
OSStatus mt_output_set_rate(const char *uid, double rate, int timeout_ms, int *notified);

#endif
