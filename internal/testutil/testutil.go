// Package testutil holds what the tests of several packages share:
// synthetic M4A and WAV files, an encoder that stands in for afconvert,
// skips for tests that need the AAC encoder or live audio, and small
// generic helpers.
//
// Only _test.go files import it. It depends on nothing in this module but
// internal/wav, so any package's tests can use it without an import cycle.
package testutil

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// EnvLiveCapture enables the tests that open real audio capture. The
// captureintegration build tag alone is not enough: tools that enable
// every build tag they find (magex test) must never open live audio.
const EnvLiveCapture = "MIXTAPE_LIVE_CAPTURE"

// Ptr returns a pointer to a copy of v.
func Ptr[T any](v T) *T { return &v }

// RequireAAC skips the test unless afconvert can encode AAC here. The AAC
// encoder is invisible inside a sandbox, where afconvert reports "aac" as
// an unknown format; afconvert -hf exits non-zero even on success, so only
// its listing counts.
func RequireAAC(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	out, _ := exec.CommandContext(t.Context(), "afconvert", "-hf").CombinedOutput()
	if !strings.Contains(string(out), "'aac '") {
		t.Skip("afconvert cannot encode AAC here (sandboxed?)")
	}
}

// RequireLive skips the test unless live capture tests were asked for
// explicitly with MIXTAPE_LIVE_CAPTURE=1. They play or capture real audio,
// so they run only with the owner present.
func RequireLive(t testing.TB) {
	t.Helper()
	if os.Getenv(EnvLiveCapture) != "1" {
		t.Skip("live capture test: set " + EnvLiveCapture + "=1 (needs Spotify playing and the owner present)")
	}
}
