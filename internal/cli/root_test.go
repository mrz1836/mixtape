package cli

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUsageErrorsExit2(t *testing.T) {
	t.Parallel()
	tests := map[string][]string{
		"unknown command":     {"bogus"},
		"unknown flag":        {"version", "--bogus"},
		"extra argument":      {"version", "extra"},
		"bad output format":   {"version", "-o", "yaml"},
		"verbose with quiet":  {"version", "-v", "-q"},
		"completion no shell": {"completion"},
		"bad completion":      {"completion", "tcsh"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			assert.Equal(t, ExitUsage, h.run(args...))
			assert.Empty(t, h.stdout.String())
			assert.Equal(t, "usage", h.errDoc()["code"])

			if name == "bad output format" {
				return // an unusable --output falls back to auto, which is JSON here
			}
			assert.Equal(t, ExitUsage, h.run(append([]string{"-o", "text"}, args...)...))
			assert.Contains(t, h.stderr.String(), "mixtape: ")
		})
	}
}

func TestUsageErrorJSON(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitUsage, h.run("-o", "json", "bogus"))
	e := h.errDoc()
	assert.Equal(t, "usage", e["code"])
	assert.InDelta(t, ExitUsage, e["exit_code"], 0)
	assert.Contains(t, e["message"], "unknown command")
	assert.Contains(t, e["hint"], "mixtape --help")
}

func TestUsageErrorText(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitUsage, h.run("-o", "text", "version", "--nope"))
	assert.Equal(t, "mixtape: version: unknown flag: --nope\n  fix: run 'mixtape version --help' for usage\n",
		h.stderr.String())
}

func TestVersion(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("version"))
	assert.Equal(t, map[string]any{"version": "1.2.3", "commit": "abc1234", "date": "2026-11-01"}, h.json(),
		"piped stdout defaults to JSON")

	h.deps.stdoutTTY = true
	require.Equal(t, ExitOK, h.run("version"))
	assert.Equal(t, "mixtape 1.2.3\ncommit:  abc1234\nbuilt:   2026-11-01\n", h.stdout.String())

	require.Equal(t, ExitOK, h.run("version", "-o", "json"))
	assert.Contains(t, h.stdout.String(), "\n  \"version\"", "JSON is indented on a terminal")
}

func TestOutputEnvDefault(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.env["MIXTAPE_OUTPUT"] = "text"
	require.Equal(t, ExitOK, h.run("version"))
	assert.Contains(t, h.stdout.String(), "mixtape 1.2.3")

	require.Equal(t, ExitOK, h.run("version", "-o", "json"), "the flag beats the environment")
	assert.Equal(t, "1.2.3", h.json()["version"])
}

func TestNoColor(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.deps.stdoutTTY, h.deps.stderrTTY = true, true

	require.Equal(t, ExitUsage, h.run("version", "--nope"))
	assert.Contains(t, h.stderr.String(), "\x1b[31m", "errors are red on a terminal")

	require.Equal(t, ExitUsage, h.run("version", "--nope", "--no-color"))
	assert.NotContains(t, h.stderr.String(), "\x1b[")

	h.env["NO_COLOR"] = "1"
	require.Equal(t, ExitUsage, h.run("version", "--nope"))
	assert.NotContains(t, h.stderr.String(), "\x1b[")
}

func TestConfigCommands(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]

	require.Equal(t, ExitOK, h.run("config", "path"))
	assert.Equal(t, filepath.Join(home, "config.json"), h.json()["path"])

	require.Equal(t, ExitOK, h.run("config", "set", "capture", "device"))
	assert.Equal(t, "capture", h.json()["key"])

	h.env["MIXTAPE_DEVICE"] = "Loopback"
	require.Equal(t, ExitOK, h.run("config", "show"))
	settings := map[string]map[string]any{}
	for _, raw := range h.json()["settings"].([]any) {
		row := raw.(map[string]any)
		settings[row["key"].(string)] = row
	}
	assert.Equal(t, "device", settings["capture"]["value"])
	assert.Equal(t, "file", settings["capture"]["source"])
	assert.Equal(t, "Loopback", settings["device"]["value"])
	assert.Equal(t, "env", settings["device"]["source"])
	assert.Equal(t, "default", settings["artwork"]["source"])

	require.Equal(t, ExitOK, h.run("config", "show", "-L", "/Volumes/X"))
	for _, raw := range h.json()["settings"].([]any) {
		if row := raw.(map[string]any); row["key"] == "library" {
			assert.Equal(t, "/Volumes/X", row["value"])
			assert.Equal(t, "flag", row["source"])
		}
	}

	require.Equal(t, ExitUsage, h.run("config", "set", "volume", "11"))
	assert.Equal(t, "usage", h.errDoc()["code"])
	require.Equal(t, ExitUsage, h.run("config", "set", "capture"))
}

func TestConfigShowBrokenFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte("{"), 0o600))

	require.Equal(t, ExitError, h.run("config", "show"))
	assert.Contains(t, h.errDoc()["hint"], "fix the JSON")

	require.Equal(t, ExitOK, h.run("version"), "commands that need no settings still work")
}

func TestConfigUnknownKeyWarns(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	home := h.env["MIXTAPE_HOME"]
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"later":1}`), 0o600))

	require.Equal(t, ExitOK, h.run("config", "show"))
	assert.Contains(t, h.stderr.String(), `{"warning":{"message":`)
	assert.Contains(t, h.stderr.String(), `\"later\"`)

	require.Equal(t, ExitOK, h.run("config", "show", "-q"))
	assert.Empty(t, h.stderr.String(), "--quiet silences warnings")
}

func TestHomeFlag(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir := t.TempDir()
	require.Equal(t, ExitOK, h.run("config", "path", "--home", dir))
	assert.Equal(t, filepath.Join(dir, "config.json"), h.json()["path"])

	require.Equal(t, ExitOK, h.run("config", "path", "--home", "~/elsewhere"))
	assert.Equal(t, filepath.Join(h.deps.userHome, "elsewhere", "config.json"), h.json()["path"])
}

func TestCompletion(t *testing.T) {
	t.Parallel()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		h := newHarness(t)
		require.Equal(t, ExitOK, h.run("completion", shell), shell)
		assert.Contains(t, h.stdout.String(), "mixtape", shell)
	}
}

func TestBannerWriterSuppression(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	a := &app{deps: h.deps}
	w := bannerWriter{a: a}

	n, err := w.Write([]byte("banner"))
	require.NoError(t, err)
	assert.Equal(t, 6, n)
	assert.Empty(t, h.stderr.String(), "nothing before the flags resolve")

	h.deps.stderrTTY = true
	a.out = &Output{json: true}
	_, _ = w.Write([]byte("banner"))
	assert.Empty(t, h.stderr.String(), "never in JSON mode")

	a.out = &Output{quiet: true}
	_, _ = w.Write([]byte("banner"))
	assert.Empty(t, h.stderr.String(), "never with --quiet")

	a.out = &Output{}
	_, _ = w.Write([]byte("banner"))
	assert.Equal(t, "banner", h.stderr.String())

	h.stderr.Reset()
	h.deps.stderrTTY = false
	_, _ = w.Write([]byte("banner"))
	assert.Empty(t, h.stderr.String(), "never when stderr is not a terminal")
}

func TestUpdateConfig(t *testing.T) {
	t.Parallel()
	cfg := updateConfig("1.0.0")
	assert.Equal(t, "mrz1836", cfg.Owner)
	assert.Equal(t, "mixtape", cfg.Repo)
	assert.Equal(t, "mixtape", cfg.BinaryName)
	assert.Equal(t, "MIXTAPE_GITHUB_TOKEN", cfg.TokenEnvVar)
	require.Len(t, cfg.Platforms, 2)
	for _, p := range cfg.Platforms {
		assert.Equal(t, "darwin", p.OS)
	}
}

func TestReleaseVersionFromBuildInfo(t *testing.T) {
	t.Parallel()
	for v, want := range map[string]bool{
		"v0.1.0":                             true,
		"v1.2.3-beta.1":                      true,
		"(devel)":                            false,
		"":                                   false,
		"v0.0.0-20261102181358-dd1387f547b6": false,
		"v0.0.0-20261102181358-dd1387f547b6+dirty": false,
		"v0.1.1-0.20261102181358-dd1387f547b6":     false,
	} {
		assert.Equal(t, want, releaseVersion(v), v)
	}
	b := BuildInfo{}.withBuildInfo(&debug.BuildInfo{
		Main:     debug.Module{Version: "v0.1.0"},
		Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}, {Key: "vcs.time", Value: "2026-11-02T00:00:00Z"}},
	})
	assert.Equal(t, BuildInfo{Version: "0.1.0", Commit: "abc", Date: "2026-11-02T00:00:00Z"}, b)
	assert.Equal(t, "1.0.0", BuildInfo{Version: "1.0.0"}.withBuildInfo(&debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}).Version,
		"-ldflags win")
}
