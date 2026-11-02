package cli

import (
	"bufio"
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/capture"
	"github.com/mrz1836/mixtape/internal/engine"
	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/session"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
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
	return run(h.t.Context(), h.deps, args)
}

// interactive makes stdin and stdout terminals, as for a person at a
// prompt (setup's guided flow, colored text output).
func (h *harness) interactive() {
	h.deps.stdinTTY, h.deps.stdoutTTY = true, true
}

// loudCapture makes every capture source deliver audible audio.
func (h *harness) loudCapture() {
	h.deps.services.openCapture = func(captureRequest) (capture.Source, error) { return &loudSource{}, nil }
}

// json decodes stdout as one JSON document.
func (h *harness) json() map[string]any {
	h.t.Helper()
	var v map[string]any
	require.NoError(h.t, json.Unmarshal(h.stdout.Bytes(), &v), "stdout: %s", h.stdout.String())
	return v
}

// ndjson decodes stdout as an event stream.
func (h *harness) ndjson() []engine.Event {
	h.t.Helper()
	var out []engine.Event
	sc := bufio.NewScanner(bytes.NewReader(h.stdout.Bytes()))
	for sc.Scan() {
		var ev engine.Event
		require.NoError(h.t, json.Unmarshal(sc.Bytes(), &ev), "line: %s", sc.Text())
		out = append(out, ev)
	}
	return out
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

// seedLibrary stores finished recordings of tracks in the collection for
// spec, as an earlier session would have.
func (h *harness) seedLibrary(spec library.Spec, tracks ...spotify.CatalogTrack) {
	h.t.Helper()
	t := h.t
	lib, err := library.Open(h.env["MIXTAPE_LIBRARY"], library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	unlock, err := lib.Lock()
	require.NoError(t, err)
	defer unlock()
	c, err := lib.Collection(spec)
	require.NoError(t, err)
	for _, s := range tracks {
		tr := library.Track{ID: s.ID, Title: s.Title, Artist: s.Artist, DurationMS: s.Duration.Milliseconds()}
		wavPath := testutil.WriteFile(t, "in.wav", make([]byte, 64))
		_, err = lib.Commit(t.Context(), c, tr, wavPath, library.CommitOptions{RecordedMS: tr.DurationMS})
		require.NoError(t, err)
	}
}

// runningID is the session runningSession starts.
const runningID = "s1"

// runningSession holds the recorder lock for session runningID, as a
// recorder in another process would, and writes its session.json. The lock
// is released when the test ends (or earlier, through the returned lock).
func (h *harness) runningSession() (*session.Lock, session.State) {
	h.t.Helper()
	id := runningID
	home := h.env["MIXTAPE_HOME"]
	lock, err := session.Acquire(h.world.lockPath, session.LockInfo{PID: 4242, SessionID: id, Home: home})
	require.NoError(h.t, err)
	h.t.Cleanup(func() { _ = lock.Release() })
	st := session.State{
		ID: id, PID: 4242, Phase: session.PhaseRecording, StartedAt: h.now(),
		Events: session.EventsPath(home, id),
	}
	require.NoError(h.t, session.WriteState(home, st))
	return lock, st
}

// findRow returns the element of doc[list] whose field is value: a doctor
// check by id, a config setting by key.
func findRow(t *testing.T, doc map[string]any, list, field, value string) map[string]any {
	t.Helper()
	rows, _ := doc[list].([]any)
	for _, r := range rows {
		if m, ok := r.(map[string]any); ok && m[field] == value {
			return m
		}
	}
	require.Failf(t, "no such row", "%s %q in %v", field, value, rows)
	return nil
}

// findCheck returns the doctor check with id.
func findCheck(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	return findRow(t, doc, "checks", "id", id)
}

// findSetting returns the `config show` row for key.
func findSetting(t *testing.T, doc map[string]any, key string) map[string]any {
	t.Helper()
	return findRow(t, doc, "settings", "key", key)
}

// testRoot builds a command tree for the help meta-tests.
func testRoot(t *testing.T) *cobra.Command {
	t.Helper()
	return newRootCmd(&app{deps: newHarness(t).deps})
}

// eventsLog writes events to the event log at path.
func (h *harness) eventsLog(path string, events ...engine.Event) {
	h.t.Helper()
	log, err := session.OpenEventLog(filepath.Clean(path))
	require.NoError(h.t, err)
	for _, ev := range events {
		require.NoError(h.t, log.Write(ev))
	}
	require.NoError(h.t, log.Close())
}
