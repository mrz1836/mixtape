//go:build integration

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The integration tests build the real binary with version ldflags and
// run it as a separate process: what scripts and agents see.
// Run them with: go test -tags integration ./internal/cli/

func buildBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mixtape")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", bin,
		"-ldflags", "-X main.version=9.9.9 -X main.commit=feedbee -X main.buildDate=2026-11-02",
		"../../cmd/mixtape")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

type proc struct {
	stdout, stderr string
	exit           int
}

func runBinary(t *testing.T, bin, home string, args ...string) proc {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), bin, args...)
	cmd.Env = append(os.Environ(), "MIXTAPE_HOME="+home, "MIXTAPE_LIBRARY="+filepath.Join(home, "lib"),
		"MIXTAPE_NO_UPDATE_CHECK=1", "NO_COLOR=1")
	var so, se bytes.Buffer
	cmd.Stdout, cmd.Stderr = &so, &se
	err := cmd.Run()
	p := proc{stdout: so.String(), stderr: se.String()}
	if ee, ok := errors.AsType[*exec.ExitError](err); ok {
		p.exit = ee.ExitCode()
	} else {
		require.NoError(t, err)
	}
	return p
}

func TestIntegrationBinaryContract(t *testing.T) {
	t.Parallel()
	bin := buildBinary(t)
	home := t.TempDir()

	t.Run("version piped is JSON with the build stamp", func(t *testing.T) {
		t.Parallel()
		p := runBinary(t, bin, home, "version")
		require.Equal(t, ExitOK, p.exit)
		assert.JSONEq(t, `{"version":"9.9.9","commit":"feedbee","date":"2026-11-02"}`, p.stdout)
		assert.Empty(t, p.stderr)

		p = runBinary(t, bin, home, "version", "-o", "text")
		assert.Equal(t, "mixtape 9.9.9\ncommit:  feedbee\nbuilt:   2026-11-02\n", p.stdout)
	})

	t.Run("help and no arguments", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{{"--help"}, {}} {
			p := runBinary(t, bin, home, args...)
			require.Equal(t, ExitOK, p.exit)
			assert.Contains(t, p.stdout, "Recording:")
			assert.Contains(t, p.stdout, "record")
		}
	})

	t.Run("usage errors exit 2 with JSON on stderr only", func(t *testing.T) {
		t.Parallel()
		for _, args := range [][]string{{"bogus"}, {"record", "not-a-link"}, {"version", "--nope"}} {
			p := runBinary(t, bin, home, args...)
			assert.Equal(t, ExitUsage, p.exit, "%v", args)
			assert.Empty(t, p.stdout, "%v", args)
			var doc struct {
				Error struct {
					Code     string `json:"code"`
					ExitCode int    `json:"exit_code"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(p.stderr), &doc), p.stderr)
			assert.Equal(t, "usage", doc.Error.Code)
			assert.Equal(t, ExitUsage, doc.Error.ExitCode)
		}
	})

	t.Run("config round trip", func(t *testing.T) {
		t.Parallel()
		cfgHome := t.TempDir()
		require.Equal(t, ExitOK, runBinary(t, bin, cfgHome, "config", "set", "capture", "device").exit)
		p := runBinary(t, bin, cfgHome, "config", "show")
		require.Equal(t, ExitOK, p.exit)
		assert.Contains(t, p.stdout, `"key":"capture","value":"device","source":"file"`)
		p = runBinary(t, bin, cfgHome, "config", "path", "-o", "text")
		assert.Equal(t, filepath.Join(cfgHome, "config.json")+"\n", p.stdout)
		assert.Equal(t, ExitUsage, runBinary(t, bin, cfgHome, "config", "set", "capture", "magic").exit)
	})

	t.Run("doctor reports JSON whatever the machine", func(t *testing.T) {
		t.Parallel()
		p := runBinary(t, bin, home, "doctor")
		assert.Contains(t, []int{ExitOK, ExitNotReady}, p.exit, p.stderr)
		var doc struct {
			Ready  bool `json:"ready"`
			Checks []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"checks"`
		}
		require.NoError(t, json.Unmarshal([]byte(p.stdout), &doc), p.stdout)
		ids := make([]string, 0, len(doc.Checks))
		for _, c := range doc.Checks {
			ids = append(ids, c.ID)
			assert.Contains(t, []string{"ok", "warn", "fail", "skip"}, c.Status)
		}
		assert.Subset(t, ids, []string{"macos", "spotify_installed", "crossfade", "afconvert", "library"})
		assert.Equal(t, doc.Ready, p.exit == ExitOK)
	})

	t.Run("list on an empty library is JSON", func(t *testing.T) {
		t.Parallel()
		p := runBinary(t, bin, home, "list")
		require.Equal(t, ExitOK, p.exit, p.stderr)
		assert.True(t, strings.HasPrefix(p.stdout, "{"))
	})
}
