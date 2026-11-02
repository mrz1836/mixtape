package spotify

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writePrefs(t *testing.T, home, rel, body string, mtime time.Time) string {
	t.Helper()
	path := filepath.Join(home, spotifySupportDir, rel)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o750))
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	require.NoError(t, os.Chtimes(path, mtime, mtime))
	return path
}

func TestReadPrefsNewestUser(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	now := time.Now()
	writePrefs(t, home, "prefs", "audio.crossfade_v2=false\n", now)
	writePrefs(t, home, "Users/old-user/prefs", "audio.crossfade_v2=false\n", now.Add(-time.Hour))
	newest := writePrefs(t, home, "Users/main-user/prefs",
		"app.player.volume=31270\naudio.gapless_migrated_to_ps=true\naudio.crossfade_v2=true\naudio.crossfade.time_v2=6658\n",
		now)

	p, err := ReadPrefs(home)
	require.NoError(t, err)
	assert.Equal(t, Prefs{Path: newest, Crossfade: true, CrossfadeSeconds: 6.658}, p)
}

func TestReadPrefsGlobalFallback(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	path := writePrefs(t, home, "prefs", `audio.automix_enabled="true"`+"\n", time.Now())

	p, err := ReadPrefs(home)
	require.NoError(t, err)
	assert.Equal(t, Prefs{Path: path, Automix: true}, p)
}

func TestReadPrefsMissing(t *testing.T) {
	t.Parallel()
	p, err := ReadPrefs(t.TempDir())
	require.NoError(t, err)
	assert.Equal(t, Prefs{}, p)
}

func TestReadPrefsMigratedIsStale(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	// The owner's real file: crossfade moved to the playback service, the
	// leftover value still says on.
	path := writePrefs(t, home, "Users/u/prefs",
		"audio.crossfade_migrated_to_ps=true\naudio.crossfade_v2=true\naudio.crossfade.time_v2=6658\n", time.Now())
	p, err := ReadPrefs(home)
	require.NoError(t, err)
	assert.Equal(t, Prefs{Path: path, Crossfade: true, CrossfadeSeconds: 6.658, Stale: true}, p)
}

func TestParsePrefs(t *testing.T) {
	t.Parallel()
	p := parsePrefs([]byte("garbage\n audio.crossfade_v2 = true \naudio.crossfade.time_v2=nope\nui.automix=false\n"))
	assert.Equal(t, Prefs{Crossfade: true}, p)
}

func TestFirstDir(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	app := filepath.Join(home, "Applications", "Spotify.app")
	file := filepath.Join(home, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	_, ok := firstDir([]string{filepath.Join(home, "missing"), file})
	assert.False(t, ok, "a plain file is not an app bundle")

	require.NoError(t, os.MkdirAll(app, 0o750))
	got, ok := firstDir([]string{filepath.Join(home, "missing"), app})
	assert.True(t, ok)
	assert.Equal(t, app, got)

	// InstalledPath consults the user's ~/Applications too.
	if _, systemWide := firstDir([]string{"/Applications/Spotify.app"}); !systemWide {
		got, ok = InstalledPath(home)
		assert.True(t, ok)
		assert.Equal(t, app, got)
	}
}

func TestIsRunningDoesNotFail(t *testing.T) {
	t.Parallel()
	// The process table is readable without any permission; whether
	// Spotify is running on this machine is not asserted.
	_, err := IsRunning()
	require.NoError(t, err)
}
