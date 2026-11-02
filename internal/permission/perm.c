//go:build darwin && cgo

// Plain C on purpose: only one package of the binary links the Objective-C
// runtime (the process tap), which keeps the linker free of duplicate
// library warnings.

#include <CoreFoundation/CoreFoundation.h>
#include <CoreServices/CoreServices.h>
#include <dispatch/dispatch.h>
#include <dlfcn.h>
#include <libproc.h>
#include <string.h>
#include <unistd.h>

#include "perm.h"

// TCC's preflight and request calls are private API, looked up at run time;
// without them every state is unknown.
typedef int (*tcc_preflight_fn)(CFStringRef, CFDictionaryRef);
typedef void (*tcc_request_fn)(CFStringRef, CFDictionaryRef, void (^)(Boolean));

static tcc_preflight_fn tcc_preflight = NULL;
static tcc_request_fn tcc_request = NULL;

static void load_tcc(void) {
    static dispatch_once_t once;
    dispatch_once(&once, ^{
      void *tcc = dlopen("/System/Library/PrivateFrameworks/TCC.framework/TCC", RTLD_LAZY);
      if (tcc != NULL) {
          tcc_preflight = (tcc_preflight_fn)dlsym(tcc, "TCCAccessPreflight");
          tcc_request = (tcc_request_fn)dlsym(tcc, "TCCAccessRequest");
      }
    });
}

// preflight maps TCCAccessPreflight: 0 granted, 1 denied, 2 undecided.
static int preflight(CFStringRef service) {
    load_tcc();
    if (tcc_preflight == NULL) {
        return -1;
    }
    int r = tcc_preflight(service, NULL);
    return (r >= 0 && r <= 2) ? r : -1;
}

int mt_perm_system_audio(void) { return preflight(CFSTR("kTCCServiceAudioCapture")); }

int mt_perm_microphone(void) { return preflight(CFSTR("kTCCServiceMicrophone")); }

int mt_perm_request_microphone(int timeout_seconds) {
    if (mt_perm_microphone() != 2 || tcc_request == NULL) {
        return mt_perm_microphone();
    }
    dispatch_semaphore_t done = dispatch_semaphore_create(0);
    tcc_request(CFSTR("kTCCServiceMicrophone"), NULL, ^(Boolean granted) {
      (void)granted;
      dispatch_semaphore_signal(done);
    });
    dispatch_semaphore_wait(done, dispatch_time(DISPATCH_TIME_NOW, (int64_t)timeout_seconds * NSEC_PER_SEC));
    dispatch_release(done);
    return mt_perm_microphone();
}

int mt_perm_automation(const char *bundle_id, int ask) {
    AEAddressDesc target;
    if (AECreateDesc(typeApplicationBundleID, bundle_id, (Size)strlen(bundle_id), &target) != noErr) {
        return -1;
    }
    OSStatus st = AEDeterminePermissionToAutomateTarget(&target, typeWildCard, typeWildCard, ask ? true : false);
    AEDisposeDesc(&target);
    switch (st) {
    case noErr:
        return 0;
    case errAEEventNotPermitted:
        return 1;
    case errAEEventWouldRequireUserConsent:
        return 2;
    case procNotFound:
        return 3;
    }
    return -1;
}

// mt_responsible_path uses responsibility_get_pid_responsible_for_pid,
// private API looked up at run time; without it this process is reported.
int mt_responsible_path(char *buf, int size) {
    pid_t (*responsible)(pid_t) =
        (pid_t (*)(pid_t))dlsym(RTLD_DEFAULT, "responsibility_get_pid_responsible_for_pid");
    pid_t pid = responsible != NULL ? responsible(getpid()) : getpid();
    if (pid <= 0 || proc_pidpath(pid, buf, (uint32_t)size) <= 0) {
        return -1;
    }
    return 0;
}
