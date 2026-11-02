package cli

import (
	"fmt"

	"github.com/mrz1836/mixtape/internal/permission"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// What recording needs is checked in three places: record (which stops at
// the first problem), doctor (which reports every one) and setup (which
// fixes what it can). The checks, their wording and their fixes live here
// so the three always agree.

// Check IDs: the "id" of each doctor check, part of the JSON contract.
// The permission checks use the permission.Kind as their ID.
const (
	checkIDMacOS          = "macos"
	checkIDConfig         = "config"
	checkIDSpotifyApp     = "spotify_installed"
	checkIDSpotifyRunning = "spotify_running"
	checkIDAutomation     = "automation"
	checkIDCrossfade      = "crossfade"
	checkIDVolume         = "volume"
	checkIDCapture        = "capture"
	checkIDTap            = "tap"
	checkIDDevice         = "device"
	checkIDRouting        = "routing"
	checkIDAfconvert      = "afconvert"
	checkIDLibrary        = "library"
	checkIDSession        = "session"
)

// Fixes, shared by the doctor checks and the error hints.
const (
	fixInstallSpotify = "install Spotify from https://www.spotify.com/download"
	fixOpenSpotify    = "open Spotify (open -a Spotify)"
	fixAfconvert      = "afconvert ships with macOS; check /usr/bin/afconvert"
	fixCrossfade      = `Spotify → Settings → Playback → turn off "Crossfade songs"`
	fixAutomix        = `Spotify → Settings → Playback → turn off "Automix"`
	fixInstallDevice  = "brew install blackhole-2ch, then 'mixtape setup'"
	fixRouting        = "create a Multi-Output Device (speakers + BlackHole) in Audio MIDI Setup and select it as the output"
	fixVolume         = "set Spotify's volume slider to the maximum (the Mac's own volume does not matter)"
)

// spotifyProcess is what a look at the process list found.
type spotifyProcess int

const (
	spotifyUnknown spotifyProcess = iota // the process list could not be read
	spotifyStopped
	spotifyRunning
)

// spotifyStatus checks whether the Spotify app is running. err is set, and
// the state unknown, when the process list could not be read.
func (a *app) spotifyStatus() (spotifyProcess, error) {
	running, err := a.deps.services.spotifyRunning()
	switch {
	case err != nil:
		return spotifyUnknown, err
	case running:
		return spotifyRunning, nil
	}
	return spotifyStopped, nil
}

// afconvertFound reports whether songs can be encoded: afconvert is on the
// PATH, or an encoder stands in for it.
func (a *app) afconvertFound() bool {
	svc := a.deps.services
	_, err := svc.lookPath("afconvert")
	return err == nil || svc.encoder != nil
}

// blending describes Spotify's crossfade or automix when either is on;
// "" when neither is, or when the prefs no longer say (Stale).
func blending(p spotify.Prefs) string {
	switch {
	case p.Stale:
		return ""
	case p.Crossfade:
		return fmt.Sprintf("crossfade is set to %.1fs", p.CrossfadeSeconds)
	case p.Automix:
		return "automix is on"
	}
	return ""
}

// blendingFix is how to turn off what blending reported.
func blendingFix(p spotify.Prefs) string {
	if p.Crossfade {
		return fixCrossfade
	}
	return fixAutomix
}

// permissionVerb says what permission k lets the host app do.
func permissionVerb(k permission.Kind) string {
	switch k {
	case permission.SystemAudio:
		return "record system audio"
	case permission.Microphone:
		return "open the capture device"
	case permission.Automation:
		return "control Spotify"
	case permission.ScreenRecording:
		return "record the screen"
	}
	return "go ahead"
}

// notReady is the error of a readiness report with n failed checks.
func notReady(n int, hint string) *Error {
	return newError(ExitNotReady, errNotReady, hint, "not ready: %d check(s) failed", n)
}
