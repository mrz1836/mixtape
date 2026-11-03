package engine

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/mp4tag"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// planned returns the action and reason of the one item for id.
func planned(t *testing.T, p *Plan, collection, id string) (Action, string) {
	t.Helper()
	for _, it := range p.Items {
		if it.Group.Spec.Name == collection && it.Track.ID == id {
			return it.Action, it.Reason
		}
	}
	t.Fatalf("%s is not planned in %s", id, collection)
	return "", ""
}

// The plan by codec: a song done in its folder is skipped whatever its
// codec; one elsewhere in the codec this run records is copied; one
// elsewhere only in another codec is recorded, with the reason, unless
// copy_codec is any.
func TestPlanByCodec(t *testing.T) {
	t.Parallel()
	s := newSim(t, songA, songB)
	s.recordAs(mp4tag.CodecAAC, library.CopySame) // as mixtape did before ALAC
	_, res := s.run(singles(songA), Options{}, nil)
	requireComplete(t, res)
	aac := s.playlist("pl0000000000000000000c", "Mix", true, songA)

	s.recordAs(mp4tag.CodecALAC, library.CopySame)
	p := BuildPlan(s.lib, singles(songA), PlanOptions{})
	action, reason := planned(t, p, SinglesFolder, songA.id)
	assert.Equal(t, ActionSkip, action, "done in its folder: a codec change never redoes it")
	assert.Empty(t, reason)

	p = BuildPlan(s.lib, []spotify.Collection{aac}, PlanOptions{})
	action, reason = planned(t, p, "Mix", songA.id)
	assert.Equal(t, ActionRecord, action, "only in AAC elsewhere: recorded in ALAC")
	assert.Equal(t, ReasonOtherCodec, reason)
	require.Len(t, p.Summary(), 1)
	assert.Equal(t, ReasonOtherCodec, p.Summary()[0].Reason, "the plan event says why")

	assert.Equal(t, ActionSkip, BuildPlan(s.lib, singles(songA), PlanOptions{Redo: false}).Items[0].Action)
	redo := BuildPlan(s.lib, singles(songA), PlanOptions{Redo: true}).Items[0]
	assert.Equal(t, ActionRecord, redo.Action, "--redo records it again, in the new codec")
	assert.Empty(t, redo.Reason)

	s.recordAs(mp4tag.CodecALAC, library.CopyAny)
	p = BuildPlan(s.lib, []spotify.Collection{aac}, PlanOptions{})
	action, reason = planned(t, p, "Mix", songA.id)
	assert.Equal(t, ActionCopy, action, "copy_codec any copies the AAC file")
	assert.Empty(t, reason)

	// Recorded in ALAC once, the song is copied in ALAC from then on, even
	// though the AAC file is still there.
	s.recordAs(mp4tag.CodecALAC, library.CopySame)
	s.resetEvents()
	_, res = s.run([]spotify.Collection{aac}, Options{}, nil)
	requireComplete(t, res)
	assert.Equal(t, 1, res.Counts.Recorded)
	saved := s.eventsOf(EventTrackSaved)
	require.Len(t, saved, 1)
	assert.Equal(t, string(mp4tag.CodecALAC), saved[0].Track.Codec, "events name the codec")
	assert.Equal(t, mp4tag.CodecALAC, s.collection(playlistSpec(aac)).Entry(songA.id).Codec)

	second := s.playlist("pl0000000000000000000d", "More", true, songA)
	p = BuildPlan(s.lib, []spotify.Collection{second}, PlanOptions{})
	action, _ = planned(t, p, "More", songA.id)
	require.Equal(t, ActionCopy, action)
	mix := s.collection(playlistSpec(aac))
	assert.Equal(t, mix.Folder()+"/"+mix.Entry(songA.id).File, p.Summary()[0].From, "copied from the ALAC file")

	s.resetEvents()
	_, res = s.run([]spotify.Collection{second}, Options{}, nil)
	requireComplete(t, res)
	copied := s.eventsOf(EventTrackCopied)
	require.Len(t, copied, 1)
	assert.Equal(t, string(mp4tag.CodecALAC), copied[0].Track.Codec)
}

// A song decided on the fly (live mode) follows the same rule as a plan:
// found only in another codec, it is recorded.
func TestLiveLookupByCodec(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		policy library.CopyPolicy
		copied int // songs copied before the one recording that ends the session
	}{
		"same: recorded again": {library.CopySame, 0},
		"any: copied":          {library.CopyAny, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSim(t, songA, songB)
			pl := s.playlist("pl0000000000000000000l", "Earlier", true, songA)
			_, res := s.run([]spotify.Collection{pl}, Options{}, nil) // AAC
			requireComplete(t, res)

			s.recordAs(mp4tag.CodecALAC, tc.policy)
			s.resetEvents()
			s.at[s.now().Sub(s.start)+time.Second] = func(s *sim) {
				s.spotify.user(func(f *fakeSpotify) {
					f.queue, f.idx, f.pos, f.playing = []string{songA.id, songB.id}, 0, 0, true
				})
			}
			_, res = s.run(nil, Options{Limit: 1}, nil)
			requireComplete(t, res)
			assert.Equal(t, tc.copied, res.Counts.Copied)
			assert.Equal(t, 1, res.Counts.Recorded)
			c := s.collection(singlesSpec())
			want := mp4tag.CodecALAC
			if tc.copied == 1 {
				want = mp4tag.CodecAAC // the copy keeps its codec
			}
			assert.Equal(t, want, c.Entry(songA.id).Codec)
		})
	}
}
