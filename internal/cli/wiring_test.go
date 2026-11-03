package cli

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/permission"
)

// TestEveryProductionServiceIsWired guards newProcessServices: a service
// left nil would only panic the first time a command reaches for it. It
// builds the real dependencies without calling anything that touches
// Spotify, audio hardware or the process's signals.
func TestEveryProductionServiceIsWired(t *testing.T) {
	t.Parallel()
	d := newProcessDeps(BuildInfo{Version: "1.2.3", Commit: "abc", Date: "2026-11-01"})
	assert.Equal(t, BuildInfo{Version: "1.2.3", Commit: "abc", Date: "2026-11-01"}, d.info, "-ldflags win")
	assert.Same(t, os.Stdout, d.stdout)
	assert.Same(t, os.Stderr, d.stderr)
	assert.Same(t, os.Stdin, d.stdinFile, "raw-mode keys read the real terminal")
	require.NotNil(t, d.now)
	require.NotNil(t, d.getenv)

	v := reflect.ValueOf(d.services)
	for i := range v.NumField() {
		f := v.Type().Field(i)
		if f.Type.Kind() == reflect.Func && f.Name != "encoder" {
			assert.False(t, v.Field(i).IsNil(), "service %s is not wired", f.Name)
		}
	}
	assert.Nil(t, d.services.encoder, "production encodes with afconvert")
	assert.NotNil(t, d.services.player(), "constructing the player runs no AppleScript")
	assert.NotNil(t, d.services.catalog())
	assert.False(t, isTerminal(nil))
}

func TestProductionCaptureRefusesAnUnselectedMethod(t *testing.T) {
	t.Parallel()
	_, err := openCapture(t.Context(), captureRequest{Method: capture.MethodAuto})
	require.ErrorIs(t, err, capture.ErrUnknownMethod, "auto must be resolved to tap or device first")
	assert.Equal(t, ExitUsage, mapErr(err).Exit)
}

func TestProductionSleepEndsWithItsContext(t *testing.T) {
	t.Parallel()
	sleep := newProcessServices().sleep
	assert.True(t, sleep(t.Context(), time.Millisecond), "a full sleep")

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	start := time.Now()
	assert.False(t, sleep(ctx, time.Hour), "a canceled wait reports it")
	assert.Less(t, time.Since(start), time.Minute, "and returns at once")
}

func TestSourceBuildsReportDevVersions(t *testing.T) {
	t.Parallel()
	b := BuildInfo{}.resolved()
	assert.Equal(t, "dev", b.Version, "a test binary is not a release")
	assert.NotEmpty(t, b.Commit, "unknown, or the VCS revision")
	assert.NotEmpty(t, b.Date)
	assert.Equal(t, BuildInfo{Version: "1.0.0", Commit: "c", Date: "d"}, BuildInfo{Version: "1.0.0", Commit: "c", Date: "d"}.resolved())
}

func TestEarlyErrorsHonorEverySpellingOfOutput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"version", "--nope", "--output=text"},
		{"version", "--nope", "-otext"},
		{"version", "--nope", "-o", "text"},
	} {
		h := newHarness(t)
		require.Equal(t, ExitUsage, h.run(args...))
		assert.Equal(t, "mixtape: version: unknown flag: --nope\n  fix: run 'mixtape version --help' for usage\n",
			h.stderr.String(), "%v", args)
	}
}

func TestHomeDefaultsToTheUsersMixtapeFolder(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	delete(h.env, "MIXTAPE_HOME")
	require.Equal(t, ExitOK, h.run("config", "path"))
	assert.Equal(t, filepath.Join(h.deps.userHome, ".mixtape", "config.json"), h.json()["path"])
}

func TestConfigText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["MIXTAPE_DEVICE"] = "Loopback"
	require.Equal(t, ExitOK, h.run("config", "show", "-o", "text"))
	out := h.stdout.String()
	assert.Regexp(t, `^KEY\s+VALUE\s+SOURCE\n`, out)
	assert.Regexp(t, `\ndevice\s+Loopback\s+env\n`, out)
	assert.Regexp(t, `\ncapture\s+auto\s+default\n`, out)
	assert.Contains(t, out, "\nfile: "+filepath.Join(h.env["MIXTAPE_HOME"], "config.json")+"\n")

	require.Equal(t, ExitOK, h.run("config", "set", "capture", "device", "-o", "text"))
	assert.Equal(t, "capture = device\n", h.stdout.String())
	require.Equal(t, ExitOK, h.run("config", "set", "capture", "", "-o", "text"))
	assert.Equal(t, "capture reset to its default\n", h.stdout.String())
	require.Equal(t, ExitOK, h.run("config", "show"))
	assert.Equal(t, "default", findSetting(t, h.json(), "capture")["source"], "the key is gone from the file")
}

func TestPermissionVerbs(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "record the screen", permissionVerb(permission.ScreenRecording))
	assert.Equal(t, "go ahead", permissionVerb(permission.Kind("bluetooth")), "a kind without its own words")
}

func TestPCMPeakCountsNegativeSwings(t *testing.T) {
	t.Parallel()
	assert.Equal(t, 4000, pcmPeak(pcmBlock(4, -4000&0xffff)), "a negative sample is as loud as a positive one")
	assert.Equal(t, 1<<15, pcmPeak([]byte{0x00, 0x80}), "the most negative sample")
	assert.Zero(t, pcmPeak([]byte{0x7f}), "a torn sample is ignored")
}

func TestUsageErrorReadsAsItsCause(t *testing.T) {
	t.Parallel()
	u := &usageError{cmdPath: "mixtape record", err: errInvalidFlag}
	assert.Equal(t, errInvalidFlag.Error(), u.Error())
	assert.ErrorIs(t, u, errInvalidFlag)
}
