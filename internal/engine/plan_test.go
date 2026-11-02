package engine

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

func TestPlanForAnAlbumNumbersItsSongsAndFlagsUnplayableOnes(t *testing.T) {
	t.Parallel()
	s := newSim(t)
	album := spotify.Collection{
		Ref: spotify.Ref{Kind: spotify.KindAlbum, ID: "al0000000000000000000a"}, Name: "Greatest", Owner: "The Band",
		Source: spotify.SourceEmbed, Complete: true,
		Tracks: []spotify.CatalogTrack{
			{ID: songA.id, Title: songA.name, Artist: "The Band", Duration: songA.dur, Playable: true, Position: 1},
			{ID: songA.id, Title: songA.name, Artist: "The Band", Duration: songA.dur, Playable: true, Position: 2},
			{ID: songB.id, Title: songB.name, Artist: "The Band", Duration: songB.dur, Position: 3},
		},
	}

	plan := BuildPlan(s.lib, []spotify.Collection{album}, PlanOptions{})
	assert.Equal(t, ModeQueue, plan.Mode)
	require.Len(t, plan.Groups, 1)
	g := plan.Groups[0]
	assert.Equal(t, library.KindAlbum, g.Spec.Kind)
	assert.Equal(t, "The Band", g.Spec.Artist, "album folders carry the artist")
	assert.Equal(t, album.Ref.URI(), g.ContextURI)
	assert.False(t, g.Follow)
	require.Len(t, plan.Items, 2, "a song listed twice is planned once")
	first, second := plan.Items[0].Track, plan.Items[1].Track
	assert.Equal(t, "Greatest", first.Album)
	assert.Equal(t, "The Band", first.AlbumArtist)
	assert.Equal(t, 1, first.TrackNumber)
	assert.Equal(t, 3, second.TrackNumber, "the album's numbering, not the plan's")
	assert.Empty(t, plan.Items[0].Hint)
	assert.Equal(t, HintUnavailable, plan.Items[1].Hint, "still planned: the catalog's word is only a hint")
	assert.Equal(t, 2, plan.Count(ActionRecord))
}

func TestPlanPutsTrackLinksIntoTheNamedFolder(t *testing.T) {
	t.Parallel()
	s := newSim(t)
	spec := library.Spec{Kind: library.KindFolder, Name: "Road Mix"}

	plan := BuildPlan(s.lib, singles(songA, songB), PlanOptions{Into: spec.Name})
	require.Len(t, plan.Groups, 1)
	assert.Equal(t, spec, plan.Groups[0].Spec)
	assert.Len(t, plan.Groups[0].Items, 2)
	require.NoError(t, Prepare(s.lib, plan))
	c := s.collection(spec)
	assert.Equal(t, "Road-Mix", c.Folder())
	assert.Equal(t, library.StatusPending, c.Entry(songB.id).Status)
	assert.Equal(t, 2, c.Entry(songB.id).Position)
}

func TestPlanRedoRecordsDoneSongsAgain(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)

	assert.Equal(t, ActionSkip, BuildPlan(s.lib, singles(songA), PlanOptions{}).Items[0].Action)
	assert.Equal(t, ActionRecord, BuildPlan(s.lib, singles(songA), PlanOptions{Redo: true}).Items[0].Action)
}

func TestPlanSummaryNamesWhereEachCopyComesFrom(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	p1 := s.playlist("pl0000000000000000001a", "One", true, songA, songB)
	p2 := s.playlist("pl0000000000000000002a", "Two", true, songB)

	sum := BuildPlan(s.lib, []spotify.Collection{p1, p2}, PlanOptions{}).Summary()
	require.Len(t, sum, 3)
	c := s.collection(singlesSpec())
	assert.Equal(t, string(ActionCopy), sum[0].Action)
	assert.Equal(t, c.Folder()+"/"+c.Entry(songA.id).File, sum[0].From, "a file already in the library")
	assert.Equal(t, string(ActionRecord), sum[1].Action)
	assert.Empty(t, sum[1].From)
	assert.Equal(t, string(ActionCopy), sum[2].Action)
	assert.Equal(t, "One", sum[2].From, "the folder that records it this session")
	assert.Equal(t, "Two", sum[2].Collection)
	assert.Equal(t, 1, sum[2].Position)
}

func TestPrepareReportsAFolderItCannotWrite(t *testing.T) {
	t.Parallel()
	skipAsRoot(t)
	for name, tc := range map[string]struct {
		lockDir func(s *sim) string
		wantErr string
	}{
		"no new folder in the library": {
			lockDir: func(s *sim) string { return s.root },
			wantErr: "prepare Singles: create collection folder",
		},
		"no manifest in the folder": {
			lockDir: func(s *sim) string {
				require.NoError(s.t, Prepare(s.lib, BuildPlan(s.lib, singles(songA), PlanOptions{})))
				return s.collection(singlesSpec()).Dir
			},
			wantErr: "prepare Singles: save manifest",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA)
			dir := tc.lockDir(s)
			require.NoError(t, os.Chmod(dir, 0o500))
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

			err := Prepare(s.lib, BuildPlan(s.lib, singles(songA, songB), PlanOptions{}))
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}
