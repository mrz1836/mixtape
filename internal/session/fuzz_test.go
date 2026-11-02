package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/testutil"
)

// stateSeeds returns session.json documents to start fuzzing from: a full
// one as WriteState renders it, the shapes the tests use, and broken ones.
func stateSeeds(tb testing.TB) [][]byte {
	tb.Helper()
	code := 0
	finished := time.Unix(30, 0).UTC()
	full, err := json.MarshalIndent(State{
		Schema: StateSchema, ID: "20261102-120000-abcdef", PID: 99, Phase: PhaseFinished, Mode: "queue",
		StartedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(20, 0).UTC(), FinishedAt: &finished,
		Library: "/Music/Mixtape", Capture: "tap", Targets: []string{"spotify:playlist:37i9dQZF1DXcBWIGoYBM5M"},
		Current: &Current{TrackID: "4cOdK2wGLETKBW3PvgPWqT", Title: "Song", Position: 12.5, Duration: 213.5, Recording: true},
		Counts:  Counts{Total: 3, Recorded: 2, Failed: 1}, ETASeconds: 60, Result: ResultIncomplete, ExitCode: &code,
		Events: "/h/sessions/x.ndjson", Log: "/h/sessions/x.log",
		Spotify: &SpotifyState{
			Original: PlayerSettings{Shuffle: testutil.Ptr(true), Repeat: testutil.Ptr(false), Volume: testutil.Ptr(40)},
			Set:      PlayerSettings{Shuffle: testutil.Ptr(false), Repeat: testutil.Ptr(true), Volume: testutil.Ptr(100)},
		},
	}, "", "  ")
	require.NoError(tb, err)
	return [][]byte{
		full,
		[]byte(`{"schema":1,"session_id":"s","phase":"recording","started_at":"2026-11-02T15:30:45+01:00"}`),
		[]byte(`{"targets":[],"current":null,"exit_code":null,"finished_at":null}`),
		[]byte(`{"started_at":"not a time"}`),
		[]byte(`{"pid":"99"}`),
		[]byte("{"),
		[]byte("null"),
		[]byte(""),
	}
}

// FuzzReadState checks ReadState on any session.json content. status,
// wait and stop read the file another process writes, so a torn or
// hand-edited file must come back as an error, never a panic, and never
// as "no session": the file is there.
func FuzzReadState(f *testing.F) {
	for _, seed := range stateSeeds(f) {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		home := t.TempDir()
		require.NoError(t, os.WriteFile(StatePath(home), data, 0o600))
		s, err := ReadState(home)
		if err != nil {
			require.NotErrorIs(t, err, ErrNoSession)
			assert.Equal(t, State{}, s)
			assert.True(t, strings.HasPrefix(err.Error(), "read "+StatePath(home)+": "), err.Error())
			require.Error(t, errors.Unwrap(err), "the decode error is wrapped")
			if !json.Valid(data) {
				var syntaxErr *json.SyntaxError
				require.ErrorAs(t, err, &syntaxErr)
			}
			return
		}

		// Whatever was read can be written back, as the recorder does on
		// every update, and reads back as the same document.
		s.Schema = StateSchema
		want, err := json.Marshal(s)
		require.NoError(t, err)
		other := filepath.Join(home, "rewritten")
		require.NoError(t, WriteState(other, s))
		back, err := ReadState(other)
		require.NoError(t, err)
		got, err := json.Marshal(back)
		require.NoError(t, err)
		assert.JSONEq(t, string(want), string(got))
	})
}

// FuzzReadLockInfo checks the lock holder record on any lock file
// content. Acquire and Inspect read it from a file another process
// writes, and it may be torn or hold a stray first line.
func FuzzReadLockInfo(f *testing.F) {
	full, err := json.Marshal(LockInfo{PID: 4242, SessionID: "s1", Home: "/h", StartedAt: time.Unix(1, 0).UTC()})
	require.NoError(f, err)
	f.Add(full)
	f.Add([]byte(`{"pid":7}`))
	f.Add([]byte(`{"pid":"7"}`))
	f.Add([]byte(`{"pid":4242,"session_id":"s1"`))
	f.Add([]byte(""))
	f.Add([]byte(strings.Repeat(" ", 4096) + `{"pid":1}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "recorder.lock")
		require.NoError(t, os.WriteFile(path, data, 0o600))
		file, err := os.Open(path)
		require.NoError(t, err)
		t.Cleanup(func() { _ = file.Close() })

		info, err := readLockInfo(file)
		if len(data) == 0 {
			require.NoError(t, err, "an empty lock file names no holder")
			assert.Equal(t, LockInfo{}, info)
			return
		}
		if err != nil {
			assert.Equal(t, LockInfo{}, info, "a failed read names no holder")
			assert.Contains(t, err.Error(), "read lock holder")
			return
		}
		// Only the first 4 KiB are read; they alone must be the record.
		assert.True(t, json.Valid(data[:min(len(data), 4096)]), "accepted a holder that is not JSON")
	})
}
