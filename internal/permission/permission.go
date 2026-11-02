// Package permission reports and requests the macOS privacy permissions
// recording needs. macOS grants them to the app it holds responsible for
// mixtape (the terminal or IDE that runs it), never to mixtape itself, and
// no program can grant them to itself: the most this package can do is
// check, show the system prompt where macOS allows one, and point at the
// exact place in System Settings.
package permission

import (
	"errors"
	"path/filepath"
	"strings"
)

// Kind is a privacy permission.
type Kind string

// Permissions mixtape uses.
const (
	// SystemAudio lets a Core Audio process tap hear other apps
	// (kTCCServiceAudioCapture): the "System Audio Recording Only" list.
	SystemAudio Kind = "system_audio_recording"
	// Microphone lets a capture device such as BlackHole be opened.
	Microphone Kind = "microphone"
	// Automation lets mixtape control Spotify over Apple Events.
	Automation Kind = "automation"
	// ScreenRecording is the top list of the same Settings pane. mixtape
	// does not need it; it is checked only because people switch the
	// terminal on there and expect that to cover the tap. It does not.
	ScreenRecording Kind = "screen_recording"
)

// State is what macOS answers for a permission.
type State string

// Permission states.
const (
	Granted   State = "granted"
	Denied    State = "denied"
	Undecided State = "undecided" // never asked: macOS behaves as denied
	Unknown   State = "unknown"   // the state could not be read
)

// ErrNotGranted reports a permission the host app does not have.
var ErrNotGranted = errors.New("macOS permission not granted")

// Settings deep links (Privacy & Security anchors).
const settingsBase = "x-apple.systempreferences:com.apple.preference.security?"

// SettingsURL opens System Settings at the list that grants k.
func SettingsURL(k Kind) string {
	switch k {
	case SystemAudio:
		return settingsBase + "Privacy_AudioCapture"
	case Microphone:
		return settingsBase + "Privacy_Microphone"
	case Automation:
		return settingsBase + "Privacy_Automation"
	}
	return settingsBase + "Privacy"
}

// Where names the place in System Settings that grants k.
func Where(k Kind) string {
	switch k {
	case SystemAudio:
		return "System Settings → Privacy & Security → Screen & System Audio Recording → " +
			"System Audio Recording Only (the bottom list, not the screen-recording one)"
	case Microphone:
		return "System Settings → Privacy & Security → Microphone"
	case Automation:
		return "System Settings → Privacy & Security → Automation → Spotify"
	}
	return "System Settings → Privacy & Security"
}

// HostApp is the app macOS holds responsible for this process.
type HostApp struct {
	Name string // e.g. "iTerm", "Terminal", "Visual Studio Code"
	Path string // the .app bundle, or the executable when not in a bundle
}

// String is the app's name for messages.
func (a HostApp) String() string {
	if a.Name == "" {
		return "the app that runs mixtape"
	}
	return a.Name
}

// hostFromPath derives the app from the responsible executable's path:
// the outermost ".app" component names it.
func hostFromPath(exe string) HostApp {
	if exe == "" {
		return HostApp{}
	}
	parts := strings.Split(filepath.Clean(exe), string(filepath.Separator))
	for i, p := range parts {
		if name, ok := strings.CutSuffix(p, ".app"); ok {
			return HostApp{Name: name, Path: string(filepath.Separator) + filepath.Join(parts[:i+1]...)}
		}
	}
	return HostApp{Name: filepath.Base(exe), Path: exe}
}

// Instructions is the fix for a permission the host app lacks.
func Instructions(k Kind, app HostApp) string {
	switch k {
	case SystemAudio:
		return "add " + app.String() + " under " + Where(k) + " and switch it on"
	case Microphone:
		return "allow " + app.String() + " under " + Where(k)
	case Automation:
		return "allow " + app.String() + " to control Spotify under " + Where(k)
	}
	return "allow " + app.String() + " under " + Where(k)
}

func fromCode(code int) State {
	switch code {
	case 0:
		return Granted
	case 1:
		return Denied
	case 2:
		return Undecided
	}
	return Unknown
}
