//go:build darwin && cgo

package tap

// The binary carries an Info.plist with NSAudioCaptureUsageDescription, the
// sentence macOS shows when it asks to let the host app record system
// audio. It lives in this one file so it can be dropped if the live tests
// show macOS does not need it for a command-line tool.

// #cgo LDFLAGS: -Wl,-sectcreate,__TEXT,__info_plist,${SRCDIR}/Info.plist
import "C"
