//go:build darwin && cgo

#import <CoreAudio/AudioHardwareTapping.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/CoreAudio.h>
#import <Foundation/Foundation.h>

#include <stdatomic.h>

#include "ctap.h"

#define MT_MAX_PLANES 16

static const AudioObjectPropertyAddress kProcessListAddr = {
    kAudioHardwarePropertyProcessObjectList, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
static const AudioObjectPropertyAddress kDefaultOutputAddr = {
    kAudioHardwarePropertyDefaultOutputDevice, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
static const AudioObjectPropertyAddress kNominalRateAddr = {
    kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};

int mt_tap_supported(void) {
    if (@available(macOS 14.2, *)) {
        return 1;
    }
    return 0;
}

// copyString reads a CFString property of obj; the caller owns the result.
static NSString *copyString(AudioObjectID obj, AudioObjectPropertySelector sel, AudioObjectPropertyScope scope,
                            OSStatus *status) {
    AudioObjectPropertyAddress addr = {sel, scope, kAudioObjectPropertyElementMain};
    CFStringRef value = NULL;
    UInt32 size = sizeof(value);
    *status = AudioObjectGetPropertyData(obj, &addr, 0, NULL, &size, &value);
    if (*status != noErr || value == NULL) {
        return nil;
    }
    return (NSString *)CFBridgingRelease(value);
}

OSStatus mt_process_bundle(AudioObjectID id, char *buf, int len) {
    if (len <= 0) {
        return kAudioHardwareBadPropertySizeError;
    }
    buf[0] = 0;
    OSStatus st = noErr;
    NSString *bundle = copyString(id, kAudioProcessPropertyBundleID, kAudioObjectPropertyScopeGlobal, &st);
    if (bundle != nil) {
        [bundle getCString:buf maxLength:(NSUInteger)len encoding:NSUTF8StringEncoding];
    }
    return st;
}

OSStatus mt_list_processes(const char *prefix, AudioObjectID *ids, int32_t *pids, int max, int *count) {
    *count = 0;
    if (@available(macOS 14.2, *)) {
    } else {
        return kAudioHardwareUnsupportedOperationError;
    }
    UInt32 size = 0;
    OSStatus st = AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &kProcessListAddr, 0, NULL, &size);
    if (st != noErr) {
        return st;
    }
    NSMutableData *all = [NSMutableData dataWithLength:size];
    st = AudioObjectGetPropertyData(kAudioObjectSystemObject, &kProcessListAddr, 0, NULL, &size, all.mutableBytes);
    if (st != noErr) {
        return st;
    }
    NSString *want = [NSString stringWithUTF8String:prefix];
    const AudioObjectID *objs = all.bytes;
    UInt32 n = size / sizeof(AudioObjectID);
    for (UInt32 i = 0; i < n && *count < max; i++) {
        OSStatus bst = noErr;
        NSString *bundle = copyString(objs[i], kAudioProcessPropertyBundleID, kAudioObjectPropertyScopeGlobal, &bst);
        if (bundle == nil || ![bundle hasPrefix:want]) {
            continue;
        }
        pid_t pid = -1;
        AudioObjectPropertyAddress pidAddr = {kAudioProcessPropertyPID, kAudioObjectPropertyScopeGlobal,
                                              kAudioObjectPropertyElementMain};
        UInt32 psize = sizeof(pid);
        if (AudioObjectGetPropertyData(objs[i], &pidAddr, 0, NULL, &psize, &pid) != noErr) {
            pid = -1;
        }
        ids[*count] = objs[i];
        pids[*count] = pid;
        (*count)++;
    }
    return noErr;
}

// inputBufferCount is the number of input streams of device: in the
// aggregate's input buffer list they come before the tap's buffers.
static UInt32 inputBufferCount(AudioObjectID device) {
    AudioObjectPropertyAddress addr = {kAudioDevicePropertyStreamConfiguration, kAudioObjectPropertyScopeInput,
                                       kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(device, &addr, 0, NULL, &size) != noErr || size == 0) {
        return 0;
    }
    NSMutableData *data = [NSMutableData dataWithLength:size];
    if (AudioObjectGetPropertyData(device, &addr, 0, NULL, &size, data.mutableBytes) != noErr) {
        return 0;
    }
    return ((const AudioBufferList *)data.bytes)->mNumberBuffers;
}

@interface MTTap : NSObject {
  @public
    mt_ring ring;
    AudioObjectID tapID;
    AudioObjectID aggID;
    AudioObjectID outputID;
    AudioDeviceIOProcID procID;
    BOOL started;
    double rate;
    uint32_t channels;
    UInt32 skipBuffers;
    _Atomic uint32_t events;
    dispatch_queue_t ioQueue;
    dispatch_queue_t listenerQueue;
    AudioObjectPropertyListenerBlock processListener;
    AudioObjectPropertyListenerBlock outputListener;
    AudioObjectPropertyListenerBlock rateListener;
}
@end

@implementation MTTap
@end

// teardown releases whatever open managed to create, in reverse order.
static void teardown(MTTap *t) {
    if (t->processListener != nil) {
        AudioObjectRemovePropertyListenerBlock(kAudioObjectSystemObject, &kProcessListAddr, t->listenerQueue,
                                               t->processListener);
        t->processListener = nil;
    }
    if (t->outputListener != nil) {
        AudioObjectRemovePropertyListenerBlock(kAudioObjectSystemObject, &kDefaultOutputAddr, t->listenerQueue,
                                               t->outputListener);
        t->outputListener = nil;
    }
    if (t->rateListener != nil && t->aggID != kAudioObjectUnknown) {
        AudioObjectRemovePropertyListenerBlock(t->aggID, &kNominalRateAddr, t->listenerQueue, t->rateListener);
        t->rateListener = nil;
    }
    if (t->started) {
        AudioDeviceStop(t->aggID, t->procID);
        t->started = NO;
    }
    if (t->procID != NULL) {
        AudioDeviceDestroyIOProcID(t->aggID, t->procID);
        t->procID = NULL;
    }
    if (t->ioQueue != nil) {
        dispatch_sync(t->ioQueue, ^{
                      }); // let an in-flight IO block finish before the ring goes away
    }
    if (t->aggID != kAudioObjectUnknown) {
        AudioHardwareDestroyAggregateDevice(t->aggID);
        t->aggID = kAudioObjectUnknown;
    }
    if (@available(macOS 14.2, *)) {
        if (t->tapID != kAudioObjectUnknown) {
            AudioHardwareDestroyProcessTap(t->tapID);
            t->tapID = kAudioObjectUnknown;
        }
    }
    mt_ring_free(&t->ring);
}

#define FAIL(st, name)        \
    do {                      \
        *status = (st);       \
        *stage = (name);      \
        teardown(t);          \
        return NULL;          \
    } while (0)

void *mt_tap_open(const AudioObjectID *ids, int count, int mute, double ring_seconds, OSStatus *status,
                  const char **stage) {
    *status = noErr;
    *stage = "";
    MTTap *t = [[MTTap alloc] init];
    t->tapID = kAudioObjectUnknown;
    t->aggID = kAudioObjectUnknown;
    t->outputID = kAudioObjectUnknown;
    mt_host_ticks_to_ns(0); // initialize the timebase off the real-time thread

    if (@available(macOS 14.2, *)) {
    } else {
        FAIL(kAudioHardwareUnsupportedOperationError, "check macOS version");
    }
    if (count <= 0) {
        FAIL(kAudioHardwareBadObjectError, "find Spotify's audio process");
    }

    NSMutableArray<NSNumber *> *procs = [NSMutableArray arrayWithCapacity:(NSUInteger)count];
    for (int i = 0; i < count; i++) {
        [procs addObject:@(ids[i])];
    }

    OSStatus st = noErr;
    NSString *tapUID = nil;
    AudioStreamBasicDescription tapFormat = {0};
    if (@available(macOS 14.2, *)) {
        CATapDescription *desc = [[CATapDescription alloc] initStereoMixdownOfProcesses:procs];
        desc.name = @"mixtape";
        desc.UUID = [NSUUID UUID];
        desc.privateTap = YES;
        desc.muteBehavior = mute ? CATapMutedWhenTapped : CATapUnmuted;
        st = AudioHardwareCreateProcessTap(desc, &t->tapID);
        if (st != noErr) {
            t->tapID = kAudioObjectUnknown;
            FAIL(st, "create process tap");
        }
        tapUID = copyString(t->tapID, kAudioTapPropertyUID, kAudioObjectPropertyScopeGlobal, &st);
        if (tapUID == nil) {
            FAIL(st, "read tap UID");
        }
        AudioObjectPropertyAddress fmtAddr = {kAudioTapPropertyFormat, kAudioObjectPropertyScopeGlobal,
                                              kAudioObjectPropertyElementMain};
        UInt32 size = sizeof(tapFormat);
        st = AudioObjectGetPropertyData(t->tapID, &fmtAddr, 0, NULL, &size, &tapFormat);
        if (st != noErr) {
            FAIL(st, "read tap format");
        }
    }

    UInt32 size = sizeof(t->outputID);
    st = AudioObjectGetPropertyData(kAudioObjectSystemObject, &kDefaultOutputAddr, 0, NULL, &size, &t->outputID);
    if (st != noErr) {
        FAIL(st, "find the default output device");
    }
    NSString *outputUID = copyString(t->outputID, kAudioDevicePropertyDeviceUID, kAudioObjectPropertyScopeGlobal, &st);
    if (outputUID == nil) {
        FAIL(st, "read the default output UID");
    }

    NSDictionary *agg = @{
        @kAudioAggregateDeviceNameKey : @"mixtape tap",
        @kAudioAggregateDeviceUIDKey : [[NSUUID UUID] UUIDString],
        @kAudioAggregateDeviceMainSubDeviceKey : outputUID,
        @kAudioAggregateDeviceIsPrivateKey : @YES,
        @kAudioAggregateDeviceIsStackedKey : @NO,
        @kAudioAggregateDeviceTapAutoStartKey : @YES,
        @kAudioAggregateDeviceSubDeviceListKey : @[ @{@kAudioSubDeviceUIDKey : outputUID} ],
        @kAudioAggregateDeviceTapListKey : @[ @{@kAudioSubTapDriftCompensationKey : @YES, @kAudioSubTapUIDKey : tapUID} ],
    };
    st = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)agg, &t->aggID);
    if (st != noErr) {
        t->aggID = kAudioObjectUnknown;
        FAIL(st, "create aggregate device");
    }

    Float64 nominal = 0;
    size = sizeof(nominal);
    if (AudioObjectGetPropertyData(t->aggID, &kNominalRateAddr, 0, NULL, &size, &nominal) != noErr || nominal <= 0) {
        nominal = tapFormat.mSampleRate;
    }
    if (nominal <= 0) {
        FAIL(kAudioHardwareUnspecifiedError, "read the sample rate");
    }
    t->rate = nominal;
    t->channels = 2;
    t->skipBuffers = inputBufferCount(t->outputID);

    double frames = ring_seconds * nominal;
    if (frames < 4096) {
        frames = 4096;
    }
    if (mt_ring_init(&t->ring, (uint64_t)frames, t->channels) != 0) {
        FAIL(kAudioHardwareUnspecifiedError, "allocate the ring buffer");
    }

    t->ioQueue = dispatch_queue_create("mixtape.tap.io", DISPATCH_QUEUE_SERIAL);
    t->listenerQueue = dispatch_queue_create("mixtape.tap.listener", DISPATCH_QUEUE_SERIAL);

    mt_ring *ring = &t->ring;
    UInt32 skip = t->skipBuffers;
    AudioDeviceIOBlock io = ^(const AudioTimeStamp *inNow, const AudioBufferList *in, const AudioTimeStamp *inTime,
                              AudioBufferList *out, const AudioTimeStamp *outTime) {
      (void)inNow;
      (void)out;
      (void)outTime;
      if (in == NULL || in->mNumberBuffers <= skip) {
          return;
      }
      uint64_t host = (inTime != NULL && (inTime->mFlags & kAudioTimeStampHostTimeValid))
                          ? mt_host_ticks_to_ns(inTime->mHostTime)
                          : mt_host_now_ns();
      UInt32 buffers = in->mNumberBuffers - skip;
      const AudioBuffer *first = &in->mBuffers[skip];
      if (first->mData == NULL) {
          return;
      }
      if (buffers == 1) {
          UInt32 ch = first->mNumberChannels ? first->mNumberChannels : 1;
          UInt32 frameCount = first->mDataByteSize / (UInt32)(sizeof(float) * ch);
          mt_ring_write_interleaved(ring, (const float *)first->mData, frameCount, ch, host);
          return;
      }
      const float *planes[MT_MAX_PLANES];
      UInt32 n = buffers < MT_MAX_PLANES ? buffers : MT_MAX_PLANES;
      for (UInt32 i = 0; i < n; i++) {
          planes[i] = (const float *)in->mBuffers[skip + i].mData;
          if (planes[i] == NULL) {
              return;
          }
      }
      mt_ring_write_planar(ring, planes, first->mDataByteSize / (UInt32)sizeof(float), n, host);
    };

    st = AudioDeviceCreateIOProcIDWithBlock(&t->procID, t->aggID, t->ioQueue, io);
    if (st != noErr) {
        t->procID = NULL;
        FAIL(st, "create the IO block");
    }

    __unsafe_unretained MTTap *weak = t;
    t->processListener = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
      (void)n;
      (void)a;
      atomic_fetch_or(&weak->events, MT_EVENT_PROCESSES);
    };
    t->outputListener = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
      (void)n;
      (void)a;
      atomic_fetch_or(&weak->events, MT_EVENT_OUTPUT);
    };
    t->rateListener = ^(UInt32 n, const AudioObjectPropertyAddress *a) {
      (void)n;
      (void)a;
      atomic_fetch_or(&weak->events, MT_EVENT_RATE);
    };
    AudioObjectAddPropertyListenerBlock(kAudioObjectSystemObject, &kProcessListAddr, t->listenerQueue,
                                        t->processListener);
    AudioObjectAddPropertyListenerBlock(kAudioObjectSystemObject, &kDefaultOutputAddr, t->listenerQueue,
                                        t->outputListener);
    AudioObjectAddPropertyListenerBlock(t->aggID, &kNominalRateAddr, t->listenerQueue, t->rateListener);

    st = AudioDeviceStart(t->aggID, t->procID);
    if (st != noErr) {
        FAIL(st, "start the aggregate device");
    }
    t->started = YES;
    return (__bridge_retained void *)t;
}

mt_ring *mt_tap_ring(void *tap) { return &((__bridge MTTap *)tap)->ring; }

double mt_tap_rate(void *tap) { return ((__bridge MTTap *)tap)->rate; }

uint32_t mt_tap_channels(void *tap) { return ((__bridge MTTap *)tap)->channels; }

uint32_t mt_tap_take_events(void *tap) { return atomic_exchange(&((__bridge MTTap *)tap)->events, 0); }

void mt_tap_close(void *tap) {
    MTTap *t = (__bridge_transfer MTTap *)tap;
    teardown(t);
}
