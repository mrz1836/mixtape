package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
)

func TestLockAcquireInspectRelease(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "recorder.lock")

	_, held, err := Inspect(path)
	require.NoError(t, err)
	assert.False(t, held)

	info := LockInfo{PID: 4242, SessionID: "s1", Home: "/h", StartedAt: time.Unix(1, 0).UTC()}
	lock, err := Acquire(path, info)
	require.NoError(t, err)

	got, held, err := Inspect(path)
	require.NoError(t, err)
	assert.True(t, held)
	assert.Equal(t, info, got)

	_, err = Acquire(path, LockInfo{PID: 1})
	require.ErrorIs(t, err, ErrBusy)
	assert.Contains(t, err.Error(), "session s1, pid 4242")

	require.NoError(t, lock.Release())
	require.NoError(t, lock.Release(), "idempotent")
	_, held, err = Inspect(path)
	require.NoError(t, err)
	assert.False(t, held)

	again, err := Acquire(path, LockInfo{PID: 7})
	require.NoError(t, err)
	require.NoError(t, again.Release())
}

func TestStateRoundTrip(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	_, err := ReadState(home)
	require.ErrorIs(t, err, ErrNoSession)

	code := 0
	in := State{
		ID: "20261102-120000-abcdef", PID: 99, Phase: PhaseFinished, Mode: "queue",
		StartedAt: time.Unix(10, 0).UTC(), UpdatedAt: time.Unix(20, 0).UTC(),
		Counts: Counts{Total: 3, Recorded: 2, Failed: 1}, Result: ResultIncomplete, ExitCode: &code,
		Current: &Current{Title: "Song", Position: 12.5},
	}
	require.NoError(t, WriteState(home, in))
	out, err := ReadState(home)
	require.NoError(t, err)
	in.Schema = StateSchema
	assert.Equal(t, in, out)
	assert.True(t, out.Finished())

	info, err := os.Stat(StatePath(home))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, os.WriteFile(StatePath(home), []byte("{"), 0o600))
	_, err = ReadState(home)
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrNoSession)
}

func TestPaths(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/h/session.json", StatePath("/h"))
	assert.Equal(t, "/h/sessions/x.ndjson", EventsPath("/h", "x"))
	assert.Equal(t, "/h/sessions/x.log", LogPath("/h", "x"))
}

func TestNewID(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 2, 15, 30, 45, 0, time.UTC)
	a, b := NewID(now), NewID(now)
	assert.True(t, strings.HasPrefix(a, "20261102-153045-"))
	assert.Len(t, a, len("20261102-153045-")+6)
	assert.NotEqual(t, a, b)
}

func TestEventLogAndFollow(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sessions", "s.ndjson")
	log, err := OpenEventLog(path)
	require.NoError(t, err)
	assert.Equal(t, path, log.Path())
	require.NoError(t, log.Write(map[string]string{"type": "a"}))
	require.NoError(t, log.Write(map[string]string{"type": "b"}))

	var buf bytes.Buffer
	off, err := Follow(path, 0, &buf, func() bool { return false })
	require.NoError(t, err)
	assert.Equal(t, []string{`{"type":"a"}`, `{"type":"b"}`}, strings.Fields(buf.String()))

	// A partial line is held back until it is complete.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(`{"type":`)
	require.NoError(t, err)
	buf.Reset()
	rounds := 0
	off2, err := Follow(path, off, &buf, func() bool {
		rounds++
		if rounds == 1 {
			_, _ = f.WriteString("\"c\"}\n")
			return true
		}
		return false
	})
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.JSONEq(t, `{"type":"c"}`, buf.String())
	assert.Greater(t, off2, off)

	require.NoError(t, log.Close())
	require.NoError(t, log.Close())
	require.ErrorIs(t, log.Write("x"), os.ErrClosed)
}

type fakeRestorer struct {
	snap  spotify.Snapshot
	calls []string
}

func (f *fakeRestorer) Poll(context.Context) spotify.Snapshot { return f.snap }

func (f *fakeRestorer) SetShuffle(_ context.Context, on bool) error {
	f.calls = append(f.calls, "shuffle="+map[bool]string{true: "on", false: "off"}[on])
	return nil
}

func (f *fakeRestorer) SetRepeat(_ context.Context, on bool) error {
	f.calls = append(f.calls, "repeat="+map[bool]string{true: "on", false: "off"}[on])
	return nil
}

// SetVolume behaves like Spotify: most values read back one lower.
func (f *fakeRestorer) SetVolume(_ context.Context, v int) error {
	f.calls = append(f.calls, "volume")
	if v > 0 && v < 100 {
		v--
	}
	f.snap.Volume = v
	return nil
}

func TestRestoreCompareAndSet(t *testing.T) {
	t.Parallel()
	st := SpotifyState{
		Original: PlayerSettings{Shuffle: testutil.Ptr(true), Repeat: testutil.Ptr(false), Volume: testutil.Ptr(40)},
		Set:      PlayerSettings{Shuffle: testutil.Ptr(false), Repeat: testutil.Ptr(true), Volume: testutil.Ptr(100)},
	}
	assert.True(t, st.NeedsRestore())

	// Shuffle and volume still hold what we set; the user turned repeat
	// off again themselves, so it is left alone.
	p := &fakeRestorer{snap: spotify.Snapshot{Shuffle: false, Repeat: false, Volume: 100}}
	restored, err := restore(t.Context(), p, st, 0)
	require.NoError(t, err)
	assert.Equal(t, []string{"shuffle", "volume"}, restored)
	assert.Equal(t, []string{"shuffle=on", "volume", "volume"}, p.calls, "the volume is nudged once")
	assert.Equal(t, 40, p.snap.Volume, "the original volume reads back exactly")

	st.Restored = true
	assert.False(t, st.NeedsRestore())
	assert.False(t, (*SpotifyState)(nil).NeedsRestore())
}

func TestRestoreReportsWhatFailed(t *testing.T) {
	t.Parallel()
	st := SpotifyState{
		Original: PlayerSettings{Shuffle: testutil.Ptr(true), Volume: testutil.Ptr(40)},
		Set:      PlayerSettings{Shuffle: testutil.Ptr(false), Volume: testutil.Ptr(100)},
	}
	p := &failingRestorer{fakeRestorer: fakeRestorer{snap: spotify.Snapshot{Volume: 100}}}
	restored, err := restore(t.Context(), p, st, 0)
	require.ErrorIs(t, err, errSetFailed)
	assert.Contains(t, err.Error(), "restore shuffle: ")
	assert.Equal(t, []string{"volume"}, restored, "one failure does not stop the others")
}

func TestRestoreWaitsForTheVolumeOnlyWhileTheContextLives(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	st := SpotifyState{Original: PlayerSettings{Volume: testutil.Ptr(47)}, Set: PlayerSettings{Volume: testutil.Ptr(100)}}
	p := &fakeRestorer{snap: spotify.Snapshot{Volume: 100}}
	start := time.Now()
	restored, err := restore(ctx, p, st, time.Hour)
	require.NoError(t, err, "the volume was set; only the read-back was cut short")
	assert.Equal(t, []string{"volume"}, restored)
	assert.Less(t, time.Since(start), time.Minute, "a canceled restore does not wait out the settle")
	assert.Equal(t, []string{"volume"}, p.calls, "no nudging without time to check")
}

func TestToRestore(t *testing.T) {
	t.Parallel()
	v, ok := toRestore(testutil.Ptr(40), testutil.Ptr(100), 100)
	assert.True(t, ok)
	assert.Equal(t, 40, v)
	_, ok = toRestore(testutil.Ptr(40), testutil.Ptr(100), 70)
	assert.False(t, ok, "the listener changed it since: theirs wins")
	_, ok = toRestore(testutil.Ptr(true), testutil.Ptr(true), true)
	assert.False(t, ok, "nothing was changed")
	_, ok = toRestore(nil, testutil.Ptr(true), true)
	assert.False(t, ok, "the original is unknown")
}

var errSetFailed = errors.New("spotify did not answer")

// failingRestorer fails to change shuffle.
type failingRestorer struct{ fakeRestorer }

func (*failingRestorer) SetShuffle(context.Context, bool) error { return errSetFailed }

func TestRestorePollError(t *testing.T) {
	t.Parallel()
	p := &fakeRestorer{snap: spotify.Snapshot{Err: spotify.ErrNotRunning}}
	_, err := Restore(t.Context(), p, SpotifyState{Set: PlayerSettings{Volume: testutil.Ptr(1)}})
	require.ErrorIs(t, err, spotify.ErrNotRunning)
}

func TestAwaitStart(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "recorder.lock")

	err := AwaitStart(t.Context(), path, "s1", func() bool { return true })
	require.ErrorIs(t, err, ErrDetachFailed)

	lock, err := Acquire(path, LockInfo{PID: os.Getpid(), SessionID: "s1"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lock.Release() })
	require.NoError(t, AwaitStart(t.Context(), path, "s1", func() bool { return false }))

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, AwaitStart(ctx, path, "other", func() bool { return false }), context.DeadlineExceeded)
}

func TestAlive(t *testing.T) {
	t.Parallel()
	assert.True(t, Alive(os.Getpid()))
	assert.False(t, Alive(0))
	assert.False(t, Alive(-5))
}

func TestSpawn(t *testing.T) {
	t.Parallel()
	logPath := filepath.Join(t.TempDir(), "logs", "child.log")
	proc, err := Spawn("/bin/sh", []string{"-c", "echo hello-$MIXTAPE_TEST_VAR"}, []string{"MIXTAPE_TEST_VAR=x"}, logPath)
	require.NoError(t, err)
	_, err = proc.Wait()
	require.NoError(t, err)
	data, err := os.ReadFile(logPath)
	require.NoError(t, err)
	assert.Equal(t, "hello-x\n", string(data))

	_, err = Spawn(filepath.Join(t.TempDir(), "missing"), nil, nil, logPath)
	require.Error(t, err)
}
