package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
	"github.com/mrz1836/mixtape/internal/testutil"
)

// songFile returns where the library keeps track id of the collection
// for spec.
func (h *harness) songFile(spec library.Spec, id string) string {
	h.t.Helper()
	lib, err := library.Open(h.env["MIXTAPE_LIBRARY"], library.Options{Encoder: &testutil.Encoder{}, WorkDir: h.t.TempDir()})
	require.NoError(h.t, err)
	c, ok := lib.Lookup(spec)
	require.True(h.t, ok, "no collection %v", spec)
	e := c.Entry(id)
	require.NotNil(h.t, e, "no song %s", id)
	return c.Path(e)
}

// trackRow returns the `list LINK` row of track id.
func trackRow(t *testing.T, doc map[string]any, id string) map[string]any {
	t.Helper()
	return findRow(t, doc, "tracks", "id", id)
}

func TestListTextOfAnEmptyLibrary(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	require.Equal(t, ExitOK, h.run("list", "-o", "text"))
	assert.Equal(t, "the library at "+h.env["MIXTAPE_LIBRARY"]+" is empty: start with 'mixtape record <link>'\n",
		h.stdout.String())
}

func TestListCountsEachFolderBySongStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()[0])
	h.editLibrary(spec, func(c *library.Collection) {
		failed := c.Ensure(library.Track{ID: trkB, Title: "Second", Artist: "Someone"})
		failed.Status, failed.FailReason = library.StatusFailed, "spotify skipped it"
		c.Ensure(library.Track{ID: "3pending00000000000000", Title: "Later", Artist: "Band"})
		gone := c.Ensure(library.Track{ID: "4removed00000000000000", Title: "Gone", Artist: "Band"})
		gone.Status = library.StatusRemoved
	})

	require.Equal(t, ExitOK, h.run("list"))
	col := findRow(t, h.json(), "collections", "name", "Mix")
	assert.Equal(t, "playlist", col["kind"])
	assert.Equal(t, plID, col["spotify_id"])
	assert.InDelta(t, 3, col["songs"], 0, "a song that left the playlist no longer counts")
	assert.InDelta(t, 1, col["done"], 0)
	assert.InDelta(t, 1, col["failed"], 0)
	assert.InDelta(t, 1, col["pending"], 0)

	require.Equal(t, ExitOK, h.run("list", "-o", "text"))
	out := h.stdout.String()
	assert.Regexp(t, `FOLDER\s+KIND\s+DONE\s+NAME`, out)
	assert.Regexp(t, `\s+playlist\s+1/3 \(1 failed\)\s+Mix`, out)
}

func TestListLinkTextShowsEachSongsStatus(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("Mix", songs()...)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()[0])
	h.editLibrary(spec, func(c *library.Collection) {
		c.Entry(trkA).Partial = true
		// A failed song has no file: one in place would be taken back as recorded.
		failed := c.Ensure(library.Track{ID: trkB, Title: "Second", Artist: "Someone"})
		failed.Status, failed.FailReason = library.StatusFailed, "spotify skipped it"
	})

	require.Equal(t, ExitOK, h.run("list", plURL))
	doc := h.json()
	assert.Equal(t, "Mix", doc["name"])
	a := trackRow(t, doc, trkA)
	assert.Equal(t, "partial", a["status"], "a partial take is not a recording")
	assert.Equal(t, "record", a["next_action"])
	b := trackRow(t, doc, trkB)
	assert.Equal(t, "failed", b["status"])
	assert.Equal(t, "spotify skipped it", b["fail_reason"])
	assert.Equal(t, "record", b["next_action"], "a failed song is tried again")
	assert.InDelta(t, 2, doc["collection"].(map[string]any)["songs"], 0)

	require.Equal(t, ExitOK, h.run("list", plURL, "-o", "text"))
	out := h.stdout.String()
	assert.Regexp(t, `^Mix\n`, out)
	assert.Regexp(t, `\n\s+1  partial\s+Rick Astley — Never Gonna Give You Up\n`, out)
	assert.Regexp(t, `\n\s+2  failed\s+Someone — Second  \(spotify skipped it\)\n`, out)
}

func TestListFailures(t *testing.T) {
	t.Parallel()
	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	tests := map[string]struct {
		setup   func(h *harness)
		args    []string
		exit    int
		message string
		hint    string
	}{
		"not a link": {
			args: []string{"list", "hello"}, exit: ExitUsage, hint: "mixtape list --help",
		},
		"a link Spotify does not know": {
			args: []string{"list", plURL}, exit: ExitNotFound, message: plID, hint: "private playlists",
		},
		"the catalog is unreachable": {
			setup: func(h *harness) {
				h.world.addPlaylist("Mix", songs()...)
				h.world.catalog.err = fmt.Errorf("%w: offline", spotify.ErrCatalog)
			},
			args: []string{"list", plURL}, exit: ExitError, message: plID, hint: "internet connection",
		},
		"a library that is a file": {
			setup: func(h *harness) { h.env["MIXTAPE_LIBRARY"] = blocker },
			args:  []string{"list"}, exit: ExitError, message: "create library",
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			if tc.setup != nil {
				tc.setup(h)
			}
			require.Equal(t, tc.exit, h.run(tc.args...), h.stderr.String())
			e := h.errDoc()
			assert.Contains(t, e["message"], tc.message)
			if tc.hint != "" {
				assert.Contains(t, e["hint"], tc.hint)
			}
			assert.Empty(t, h.stdout.String())
		})
	}
}

func TestListSkipsAFolderWithABrokenManifest(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	dir := filepath.Join(h.env["MIXTAPE_LIBRARY"], "Broken")
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(dir, library.ManifestName), []byte("{"), 0o600))

	require.Equal(t, ExitOK, h.run("list"))
	assert.Empty(t, h.json()["collections"])
	w := h.warnings()
	require.Len(t, w, 1)
	assert.Contains(t, w[0], "skipping Broken:", "the library says what it left out")
}

func TestVerifyFindsAMissingSongAndFixPutsItBackToPending(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.world.addPlaylist("Mix", songs()...)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()...)
	gone := h.songFile(spec, trkA)
	require.NoError(t, os.Remove(gone))

	require.Equal(t, ExitError, h.run("verify"))
	rep := h.json()
	assert.InDelta(t, 2, rep["files"], 0)
	assert.InDelta(t, 1, rep["ok"], 0)
	p := findRow(t, rep, "problems", "track_id", trkA)
	assert.Equal(t, "missing", p["kind"])
	assert.Equal(t, filepath.Base(gone), p["file"])
	e := h.errDoc()
	assert.Equal(t, "error", e["code"])
	assert.Equal(t, "1 problem(s) in the library", e["message"])
	assert.Equal(t, "run 'mixtape verify --fix'", e["hint"])

	require.Equal(t, ExitError, h.run("verify", "-o", "text"))
	assert.Equal(t, "checked 2 song(s) in 1 folder(s): 1 ok\n"+
		"  missing         Mix/"+filepath.Base(gone)+": file not found; the next run records it again\n", h.stdout.String())

	require.Equal(t, ExitOK, h.run("verify", "--fix", "-o", "text"), h.stderr.String())
	assert.Contains(t, h.stdout.String(), "\nfixed 1\n")

	require.Equal(t, ExitOK, h.run("list", plURL))
	a := trackRow(t, h.json(), trkA)
	assert.Equal(t, "pending", a["status"])
	assert.Equal(t, "record", a["next_action"], "the next record run records it again")

	require.Equal(t, ExitOK, h.run("verify"), "nothing is left to fix")
}

func TestVerifyFixRefusesWhenMostSongsAreMissing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()...)
	for _, s := range songs() {
		require.NoError(t, os.Remove(h.songFile(spec, s.ID)))
	}

	require.Equal(t, ExitError, h.run("verify", "--fix"))
	e := h.errDoc()
	assert.Contains(t, e["message"], "2 of 2 songs missing")
	assert.Equal(t, "check that the library drive is mounted; --force rewrites anyway", e["hint"])
	h.editLibrary(spec, func(c *library.Collection) {
		assert.Equal(t, library.StatusRecorded, c.Entry(trkA).Status, "nothing was rewritten")
	})

	require.Equal(t, ExitOK, h.run("verify", "--fix", "--force"), h.stderr.String())
	assert.Len(t, h.json()["fixed"], 2)
}

func TestVerifyFixLeavesWhatItCannotRepair(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()...)
	// The second song's file now holds the first song (tagged for it).
	data, err := os.ReadFile(h.songFile(spec, trkA))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(h.songFile(spec, trkB), data, 0o600))

	require.Equal(t, ExitError, h.run("verify", "--fix"))
	rep := h.json()
	assert.Equal(t, "tag_mismatch", findRow(t, rep, "problems", "track_id", trkB)["kind"])
	assert.Empty(t, rep["fixed"], "a mistagged file is the owner's to sort out")
	e := h.errDoc()
	assert.Equal(t, "1 problem(s) in the library", e["message"])
	assert.Contains(t, e["hint"], "--fix leaves these to you", "the hint is about what is left, not what was fixed")
}

func TestVerifyReportsOrphansWithoutFailing(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	spec := playlistSpec("Mix")
	h.seedLibrary(spec, songs()[0])
	dir := filepath.Dir(h.songFile(spec, trkA))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "Stray.m4a"), testutil.M4A(testutil.M4AOptions{}), 0o600))

	require.Equal(t, ExitOK, h.run("verify"), "orphans alone do not fail verify")
	p := findRow(t, h.json(), "problems", "kind", "orphan")
	assert.Equal(t, "Stray.m4a", p["file"])
	assert.Empty(t, h.stderr.String())
}

func TestVerifyFixWaitsForNoOtherLibraryWriter(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.seedLibrary(playlistSpec("Mix"), songs()[0])
	lib, err := library.Open(h.env["MIXTAPE_LIBRARY"], library.Options{Encoder: &testutil.Encoder{}, WorkDir: t.TempDir()})
	require.NoError(t, err)
	unlock, err := lib.Lock()
	require.NoError(t, err)
	t.Cleanup(unlock)

	require.Equal(t, ExitOK, h.run("verify"), "checking alone needs no lock")
	require.Equal(t, ExitBusy, h.run("verify", "--fix"))
	assert.Contains(t, h.errDoc()["hint"], "another mixtape command is writing the library")
}
