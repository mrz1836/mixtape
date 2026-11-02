package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// harness runs the real command tree against in-memory streams, a private
// home directory and a fixed environment.
type harness struct {
	t      *testing.T
	stdout bytes.Buffer
	stderr bytes.Buffer
	env    map[string]string
	deps   *deps
	world  *fakeWorld
	mu     sync.Mutex
	clock  time.Time
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, env: map[string]string{
		"MIXTAPE_HOME":            t.TempDir(),
		"MIXTAPE_LIBRARY":         t.TempDir(),
		"MIXTAPE_NO_UPDATE_CHECK": "1",
	}, clock: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)}
	h.world = newFakeWorld(t)
	h.deps = &deps{
		info:     BuildInfo{Version: "1.2.3", Commit: "abc1234", Date: "2026-11-01"},
		stdin:    strings.NewReader(""),
		stdout:   &h.stdout,
		stderr:   &h.stderr,
		getenv:   func(k string) string { return h.env[k] },
		userHome: t.TempDir(),
		now:      h.now,
	}
	h.deps.services = h.world.services(h)
	return h
}

func (h *harness) now() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock
}

// advance moves the harness clock (the fake sleep calls it).
func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clock = h.clock.Add(d)
}

// run executes args and returns the exit code; output stays in the buffers.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	h.stdout.Reset()
	h.stderr.Reset()
	return run(context.Background(), h.deps, args)
}

// json decodes stdout as one JSON document.
func (h *harness) json() map[string]any {
	h.t.Helper()
	var v map[string]any
	require.NoError(h.t, json.Unmarshal(h.stdout.Bytes(), &v), "stdout: %s", h.stdout.String())
	return v
}

// errDoc decodes the JSON error document on stderr.
func (h *harness) errDoc() map[string]any {
	h.t.Helper()
	var v struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(h.t, json.Unmarshal(h.stderr.Bytes(), &v), "stderr: %s", h.stderr.String())
	require.NotNil(h.t, v.Error, "stderr: %s", h.stderr.String())
	return v.Error
}

// testRoot builds a command tree for the help meta-tests.
func testRoot(t *testing.T) *cobra.Command {
	t.Helper()
	return newRootCmd(&app{deps: newHarness(t).deps})
}
