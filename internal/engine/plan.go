package engine

import (
	"fmt"
	"os"

	"github.com/mrz1836/mixtape/internal/library"
	"github.com/mrz1836/mixtape/internal/spotify"
)

// Action is what a session does with one planned song.
type Action string

// Actions, in the order a session performs them.
const (
	// ActionSkip: the song is already in this folder.
	ActionSkip Action = "skip"
	// ActionCopy: the song is complete elsewhere in the library (or will be
	// once an earlier item of this plan records it), so it is copied and
	// retagged instead of recorded.
	ActionCopy Action = "copy"
	// ActionEncode: the song was recorded but encoding failed last time;
	// the kept WAV is encoded again.
	ActionEncode Action = "encode"
	// ActionRecord: the song is played and recorded.
	ActionRecord Action = "record"
)

// HintUnavailable marks a song the catalog says cannot play here. It is
// only a hint: playback is still attempted.
const HintUnavailable = "unavailable"

// Session modes.
const (
	ModeQueue  = "queue"  // record the given links
	ModeFollow = "follow" // a link's track list is incomplete: follow its context
	ModeLive   = "live"   // no links: record whatever plays
)

// SinglesFolder is the folder for individual tracks and live recordings.
const SinglesFolder = "Singles"

// Plan is the work a session will do.
type Plan struct {
	Mode   string
	Groups []*Group
	Items  []*Item
}

// Group is one collection in the plan: a playlist, an album, or the
// singles folder that gathers every track link.
type Group struct {
	Spec       library.Spec
	Spotify    spotify.Collection
	ContextURI string // played in this context; "" for singles
	// Follow is set when the track list is incomplete (a playlist past the
	// embed's 100-track cap, or no list at all): after the known tracks
	// the session follows the playlist until it loops.
	Follow     bool
	Collection *library.Collection // nil in a dry run for a folder that does not exist yet
	Items      []*Item
}

// Item is one planned song.
type Item struct {
	Group  *Group
	Track  library.Track
	Action Action
	From   *library.Location // copy source in the library
	Twin   *Item             // an earlier item recording the same song; copy from it
	Hint   string

	state       itemState
	attempts    int
	falseStarts int  // takes that ended before the song really began
	retake      bool // live mode: a take was cut short; record the song if it plays again
	reason      string
}

type itemState int

const (
	itemPending itemState = iota
	itemBusy              // recorded; being cut, encoded and saved
	itemDone
	itemFailed
)

// PlanOptions tunes BuildPlan.
type PlanOptions struct {
	Redo bool   // re-record songs that are already done
	Into string // folder for track links ("" = Singles)
}

// BuildPlan decides what to do with every song of the resolved
// collections without changing anything on disk. Songs keep the order of
// the links and of each collection; a song that appears twice is recorded
// once and copied to its other folders.
func BuildPlan(lib *library.Library, cols []spotify.Collection, o PlanOptions) *Plan {
	p := &Plan{Mode: ModeLive}
	seen := map[string]*Item{}
	var singles *Group
	for _, col := range cols {
		g := groupFor(col, o, &singles)
		if g == nil {
			continue // folded into the singles group
		}
		p.Groups = append(p.Groups, g)
	}
	if singles != nil {
		p.Groups = append(p.Groups, singles)
	}
	if len(cols) == 0 {
		// Live mode records into the singles folder (or --into).
		var live *Group
		groupFor(spotify.Collection{}, o, &live)
		p.Groups = append(p.Groups, live)
	}
	for _, g := range p.Groups {
		if c, ok := lib.Lookup(g.Spec); ok {
			g.Collection = c
		}
		for _, ct := range g.Spotify.Tracks {
			if g.find(ct.ID) != nil {
				continue // listed twice in one collection
			}
			it := &Item{Group: g, Track: trackFrom(g, ct)}
			if !ct.Playable && g.Spotify.Source == spotify.SourceEmbed {
				it.Hint = HintUnavailable
			}
			it.Action, it.From, it.Twin = decide(lib, g, ct.ID, seen[ct.ID], o.Redo)
			if it.Action == ActionRecord {
				seen[ct.ID] = it
			}
			g.Items = append(g.Items, it)
			p.Items = append(p.Items, it)
		}
		switch {
		case len(cols) == 0:
		case g.Follow:
			p.Mode = ModeFollow
		case p.Mode == ModeLive:
			p.Mode = ModeQueue
		}
	}
	return p
}

// groupFor maps a resolved collection to a plan group. Track links all
// share one singles group, created on first use.
func groupFor(col spotify.Collection, o PlanOptions, singles **Group) *Group {
	switch col.Ref.Kind {
	case spotify.KindAlbum:
		return &Group{
			Spec: library.Spec{
				Kind: library.KindAlbum, SpotifyID: col.Ref.ID, Name: col.Name,
				Owner: col.Owner, URL: col.Ref.URL(), Artist: col.Owner,
			},
			Spotify: col, ContextURI: col.Ref.URI(), Follow: !col.Complete,
		}
	case spotify.KindPlaylist:
		return &Group{
			Spec: library.Spec{
				Kind: library.KindPlaylist, SpotifyID: col.Ref.ID, Name: col.Name,
				Owner: col.Owner, URL: col.Ref.URL(),
			},
			Spotify: col, ContextURI: col.Ref.URI(), Follow: !col.Complete,
		}
	}
	if *singles == nil {
		spec := library.Spec{Kind: library.KindSingles, Name: SinglesFolder}
		if o.Into != "" {
			spec = library.Spec{Kind: library.KindFolder, Name: o.Into}
		}
		*singles = &Group{Spec: spec, Spotify: spotify.Collection{Source: spotify.SourceEmbed, Complete: true}}
	}
	g := *singles
	for _, ct := range col.Tracks {
		ct.Position = len(g.Spotify.Tracks) + 1
		g.Spotify.Tracks = append(g.Spotify.Tracks, ct)
	}
	if col.Source != spotify.SourceEmbed {
		g.Spotify.Source = col.Source // durations unknown: availability hints are off
	}
	return nil
}

// decide picks the action for one song.
func decide(lib *library.Library, g *Group, id string, twin *Item, redo bool) (Action, *library.Location, *Item) {
	if twin != nil {
		return ActionCopy, nil, twin
	}
	if redo {
		return ActionRecord, nil, nil
	}
	if c := g.Collection; c != nil {
		if c.Done(id) {
			return ActionSkip, nil, nil
		}
		if e := c.Entry(id); e != nil && e.Status == library.StatusEncodeFailed && fileExists(e.PendingWAV) {
			return ActionEncode, nil, nil
		}
	}
	for _, loc := range lib.Find(id) {
		if g.Collection == nil || loc.Collection.Dir != g.Collection.Dir {
			return ActionCopy, &loc, nil
		}
	}
	return ActionRecord, nil, nil
}

// trackFrom builds the library metadata known before recording.
func trackFrom(g *Group, ct spotify.CatalogTrack) library.Track {
	t := library.Track{
		ID: ct.ID, Title: ct.Title, Artist: ct.Artist, DurationMS: ct.Duration.Milliseconds(), ReleaseDate: ct.ReleaseDate,
	}
	if g.Spec.Kind == library.KindAlbum {
		t.Album, t.AlbumArtist, t.TrackNumber = g.Spotify.Name, g.Spotify.Owner, ct.Position
	}
	return t
}

func (g *Group) find(id string) *Item {
	for _, it := range g.Items {
		if it.Track.ID == id {
			return it
		}
	}
	return nil
}

// Prepare creates the plan's folders and writes every collection's
// manifest with its songs pending, so progress is visible on disk before
// anything plays.
func Prepare(lib *library.Library, p *Plan) error {
	for _, g := range p.Groups {
		c, err := lib.Collection(g.Spec)
		if err != nil {
			return fmt.Errorf("prepare %s: %w", g.Spec.Name, err)
		}
		tracks := make([]library.Track, 0, len(g.Items))
		for _, it := range g.Items {
			tracks = append(tracks, it.Track)
		}
		// Singles accumulate; a playlist or album drops songs that left it
		// only when its list is known to be complete.
		complete := g.Spotify.Complete && g.Spec.Kind != library.KindSingles && g.Spec.Kind != library.KindFolder
		c.Sync(tracks, complete)
		if err := lib.Save(c); err != nil {
			return fmt.Errorf("prepare %s: %w", g.Spec.Name, err)
		}
		g.Collection = c
	}
	return nil
}

// Summary returns the plan as event items.
func (p *Plan) Summary() []PlanItem {
	out := make([]PlanItem, 0, len(p.Items))
	for _, it := range p.Items {
		pi := PlanItem{
			Action: string(it.Action), ID: it.Track.ID, Title: it.Track.Title, Artist: it.Track.Artist,
			Collection: it.Group.Spec.Name, Position: it.position(), DurationMS: it.Track.DurationMS, Hint: it.Hint,
		}
		switch {
		case it.From != nil:
			pi.From = it.From.RelPath()
		case it.Twin != nil:
			pi.From = it.Twin.Group.Spec.Name
		}
		out = append(out, pi)
	}
	return out
}

// Count returns how many items have action a.
func (p *Plan) Count(a Action) int {
	n := 0
	for _, it := range p.Items {
		if it.Action == a {
			n++
		}
	}
	return n
}

// position is the item's 1-based place in its collection.
// unbusy puts a song whose save failed back to pending.
func (it *Item) unbusy() {
	if it.state == itemBusy {
		it.state = itemPending
	}
}

func (it *Item) position() int {
	for i, x := range it.Group.Items {
		if x == it {
			return i + 1
		}
	}
	return 0
}

func fileExists(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}
