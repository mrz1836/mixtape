//go:build darwin && cgo

package permission

/*
#cgo CFLAGS: -fblocks -mmacosx-version-min=13.0
#cgo LDFLAGS: -framework CoreServices -framework CoreFoundation
#include <stdlib.h>
#include "perm.h"
*/
import "C"

import "unsafe"

// requestTimeout bounds how long a system prompt is waited for.
const requestTimeout = 120

// Check reports k for the host app without showing any prompt.
func Check(k Kind) State {
	switch k {
	case SystemAudio:
		return fromCode(int(C.mt_perm_system_audio()))
	case Microphone:
		return fromCode(int(C.mt_perm_microphone()))
	case Automation:
		return automation(false)
	case ScreenRecording:
		return fromCode(int(C.mt_perm_screen_recording()))
	}
	return Unknown
}

// Request shows the system prompt for k when macOS allows one and the
// permission is undecided, and returns the state afterwards. System audio
// recording has no prompt for apps that do not declare it (Terminal, iTerm):
// it is checked only, and must be granted in System Settings.
func Request(k Kind) State {
	switch k {
	case Microphone:
		return fromCode(int(C.mt_perm_request_microphone(requestTimeout)))
	case Automation:
		return automation(true)
	case SystemAudio, ScreenRecording:
	}
	return Check(k)
}

func automation(ask bool) State {
	id := C.CString(SpotifyBundleID)
	defer C.free(unsafe.Pointer(id))
	flag := C.int(0)
	if ask {
		flag = 1
	}
	code := int(C.mt_perm_automation(id, flag))
	if code == 3 {
		return Unknown // Spotify is not running: macOS can only answer for a running target
	}
	return fromCode(code)
}

// App returns the app macOS holds responsible for this process.
func App() HostApp {
	buf := make([]byte, 4096)
	if C.mt_responsible_path((*C.char)(unsafe.Pointer(&buf[0])), C.int(len(buf))) != 0 {
		return HostApp{}
	}
	n := 0
	for n < len(buf) && buf[n] != 0 {
		n++
	}
	return hostFromPath(string(buf[:n]))
}
