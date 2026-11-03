package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/mrz1836/mixtape/internal/config"
	"github.com/mrz1836/mixtape/internal/mp4tag"
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
	checkIDEncoder        = "encoder"
	checkIDOutput         = "output"
	checkIDLibrary        = "library"
	checkIDSession        = "session"
)

// Fixes, shared by the doctor checks and the error hints.
const (
	fixInstallSpotify = "install Spotify from https://www.spotify.com/download"
	fixOpenSpotify    = "open Spotify (open -a Spotify)"
	fixAfconvert      = "afconvert ships with macOS; check /usr/bin/afconvert"
	fixEncoder        = "run mixtape from a normal terminal session (a sandbox hides the encoders); " +
		"a song that fails to encode keeps its WAV and is encoded on the next run"
	fixCrossfade     = `Spotify → Settings → Playback → turn off "Crossfade songs"`
	fixAutomix       = `Spotify → Settings → Playback → turn off "Automix"`
	fixInstallDevice = "brew install blackhole-2ch, then 'mixtape setup'"
	fixRouting       = "create a Multi-Output Device (speakers + BlackHole) in Audio MIDI Setup and select it as the output"
	fixVolume        = "set Spotify's volume slider to the maximum (the Mac's own volume does not matter)"
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

// afconvertCodecs lists the codecs afconvert can write into M4A files,
// from its -hf listing: the data formats under the 'm4af' file type.
// Inside a sandbox the encoders are hidden and the list is empty.
func afconvertCodecs(help string) map[mp4tag.Codec]bool {
	out := map[mp4tag.Codec]bool{}
	in := false
	for line := range strings.Lines(help) {
		if strings.Contains(line, "' = ") {
			in = strings.Contains(line, "'m4af' = ")
			continue
		}
		if !in {
			continue
		}
		if strings.Contains(line, "'alac'") {
			out[mp4tag.CodecALAC] = true
		}
		if strings.Contains(line, "'aac '") {
			out[mp4tag.CodecAAC] = true
		}
	}
	return out
}

// encoderCheck reports whether afconvert lists the encoder for the codec
// the settings record. A missing one is a warning, not a failure: the
// listing is a hint (a sandbox empties it), and a song that cannot be
// encoded keeps its WAV for the next run.
func (a *app) encoderCheck(ctx context.Context, s config.Settings) check {
	c := check{ID: checkIDEncoder, Name: "Encoder", Status: checkOK, Detail: encoderDetail(s)}
	svc := a.deps.services
	if svc.encoder != nil {
		return c // a stand-in encodes whatever it is asked
	}
	if !a.afconvertFound() {
		c.Status, c.Detail = checkSkip, "checked once afconvert is found"
		return c
	}
	if !afconvertCodecs(svc.afconvertHelp(ctx))[s.Codec] {
		c.Status, c.Fix = checkWarn, fixEncoder
		c.Detail = "afconvert does not list the " + codecLabel(string(s.Codec)) + " encoder here"
	}
	return c
}

// encoderDetail describes what the settings record.
func encoderDetail(s config.Settings) string {
	if s.Codec.Lossless() {
		return fmt.Sprintf("ALAC, lossless (%d-bit capture)", s.Bits())
	}
	return fmt.Sprintf("AAC at %d kb/s (%d-bit capture)", s.AACBitrate, s.Bits())
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
