//go:build darwin && cgo

#import <CoreAudio/CoreAudio.h>
#import <Foundation/Foundation.h>

#include "coutput.h"

static AudioObjectPropertyAddress global(AudioObjectPropertySelector sel) {
    AudioObjectPropertyAddress a = {sel, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    return a;
}

// copyCString copies a CFString property of obj into buf as UTF-8.
static OSStatus copyCString(AudioObjectID obj, AudioObjectPropertySelector sel, char *buf, int len) {
    if (len <= 0) {
        return kAudioHardwareBadPropertySizeError;
    }
    buf[0] = 0;
    AudioObjectPropertyAddress a = global(sel);
    CFStringRef value = NULL;
    UInt32 size = sizeof(value);
    OSStatus st = AudioObjectGetPropertyData(obj, &a, 0, NULL, &size, &value);
    if (st != noErr || value == NULL) {
        return st != noErr ? st : kAudioHardwareUnspecifiedError;
    }
    CFStringGetCString(value, buf, (CFIndex)len, kCFStringEncodingUTF8);
    CFRelease(value);
    return noErr;
}

// deviceForUID finds the connected device with uid.
static OSStatus deviceForUID(const char *uid, AudioObjectID *dev) {
    *dev = kAudioObjectUnknown;
    CFStringRef s = CFStringCreateWithCString(NULL, uid, kCFStringEncodingUTF8);
    if (s == NULL) {
        return MT_NO_DEVICE;
    }
    AudioObjectPropertyAddress a = global(kAudioHardwarePropertyTranslateUIDToDevice);
    UInt32 size = sizeof(*dev);
    OSStatus st = AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, sizeof(s), &s, &size, dev);
    CFRelease(s);
    if (st == noErr && *dev == kAudioObjectUnknown) {
        st = MT_NO_DEVICE;
    }
    return st;
}

static OSStatus nominalRate(AudioObjectID dev, double *rate) {
    AudioObjectPropertyAddress a = global(kAudioDevicePropertyNominalSampleRate);
    Float64 r = 0;
    UInt32 size = sizeof(r);
    OSStatus st = AudioObjectGetPropertyData(dev, &a, 0, NULL, &size, &r);
    *rate = r;
    return st;
}

OSStatus mt_output_default(char *uid, int uid_len, char *name, int name_len, double *rate, double *ranges,
                           int max_ranges, int *n_ranges) {
    *rate = 0;
    *n_ranges = 0;
    AudioObjectPropertyAddress a = global(kAudioHardwarePropertyDefaultOutputDevice);
    AudioObjectID dev = kAudioObjectUnknown;
    UInt32 size = sizeof(dev);
    OSStatus st = AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, &dev);
    if (st != noErr) {
        return st;
    }
    if (dev == kAudioObjectUnknown) {
        return MT_NO_DEVICE;
    }
    if ((st = copyCString(dev, kAudioDevicePropertyDeviceUID, uid, uid_len)) != noErr) {
        return st;
    }
    copyCString(dev, kAudioObjectPropertyName, name, name_len);
    if ((st = nominalRate(dev, rate)) != noErr) {
        return st;
    }
    AudioObjectPropertyAddress ra = global(kAudioDevicePropertyAvailableNominalSampleRates);
    size = 0;
    if (AudioObjectGetPropertyDataSize(dev, &ra, 0, NULL, &size) != noErr || size == 0) {
        return noErr; // no list: the rate it runs at is all that is known
    }
    NSMutableData *data = [NSMutableData dataWithLength:size];
    if (AudioObjectGetPropertyData(dev, &ra, 0, NULL, &size, data.mutableBytes) != noErr) {
        return noErr;
    }
    const AudioValueRange *r = data.bytes;
    int n = (int)(size / sizeof(AudioValueRange));
    for (int i = 0; i < n && i < max_ranges; i++) {
        ranges[2 * i] = r[i].mMinimum;
        ranges[2 * i + 1] = r[i].mMaximum;
        (*n_ranges)++;
    }
    return noErr;
}

OSStatus mt_output_rate(const char *uid, double *rate) {
    *rate = 0;
    AudioObjectID dev;
    OSStatus st = deviceForUID(uid, &dev);
    if (st != noErr) {
        return st;
    }
    return nominalRate(dev, rate);
}

OSStatus mt_output_set_rate(const char *uid, double rate, int timeout_ms, int *notified) {
    *notified = 0;
    AudioObjectID dev;
    OSStatus st = deviceForUID(uid, &dev);
    if (st != noErr) {
        return st;
    }
    AudioObjectPropertyAddress a = global(kAudioDevicePropertyNominalSampleRate);
    dispatch_semaphore_t changed = dispatch_semaphore_create(0);
    dispatch_queue_t q = dispatch_queue_create("mixtape.output.rate", DISPATCH_QUEUE_SERIAL);
    AudioObjectPropertyListenerBlock listener = ^(UInt32 n, const AudioObjectPropertyAddress *addr) {
      (void)n;
      (void)addr;
      dispatch_semaphore_signal(changed);
    };
    AudioObjectAddPropertyListenerBlock(dev, &a, q, listener);
    Float64 r = rate;
    st = AudioObjectSetPropertyData(dev, &a, 0, NULL, sizeof(r), &r);
    if (st == noErr &&
        dispatch_semaphore_wait(changed, dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_ms * NSEC_PER_MSEC)) == 0) {
        *notified = 1;
    }
    AudioObjectRemovePropertyListenerBlock(dev, &a, q, listener);
    dispatch_sync(q, ^{
                  }); // no listener call is left in flight
    return st;
}
