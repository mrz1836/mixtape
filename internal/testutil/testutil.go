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

// RequireEncoder skips the test unless afconvert can write codec ("aac"
// or "alac") into an M4A file here. The encoders are invisible inside a
// sandbox, where afconvert lists no data formats for M4A files at all;
// afconvert -hf exits non-zero even on success, so only its listing
// counts.
func RequireEncoder(t testing.TB, codec string) {
	t.Helper()
	if _, err := exec.LookPath("afconvert"); err != nil {
		t.Skip("afconvert not available (macOS only)")
	}
	out, _ := exec.CommandContext(t.Context(), "afconvert", "-hf").CombinedOutput()
	if !strings.Contains(m4afFormats(string(out)), "'"+fourCC(codec)+"'") {
		t.Skipf("afconvert cannot encode %s here (sandboxed?)", codec)
	}
}

// fourCC is the afconvert data format of a codec name.
func fourCC(codec string) string {
	if codec == "aac" {
		return "aac "
	}
	return codec
}

// m4afFormats is the data-format listing of the M4A file type in
// afconvert -hf output: the lines from its "'m4af' =" line up to the
// next file type.
func m4afFormats(help string) string {
	var b strings.Builder
	in := false
	for line := range strings.Lines(help) {
		if strings.Contains(line, "' = ") {
			in = strings.Contains(line, "'m4af' = ")
			continue
		}
		if in {
			b.WriteString(line)
		}
	}
	return b.String()
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
