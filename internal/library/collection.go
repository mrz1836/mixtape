package library

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Collection is one folder of the library and its manifest.
type Collection struct {
	Dir      string
	Manifest Manifest

	dirty bool // reconciled in memory, not yet saved
	// staleWAVs are kept WAVs whose songs reconcile found on disk; they are
	// removed only by a writer, once the manifest no longer needs them.
	staleWAVs []string
}

// Folder returns the folder's base name.
func (c *Collection) Folder() string { return filepath.Base(c.Dir) }

// Entry returns the track whose requested or played ID is id, or nil. The
// pointer is valid until the collection's track list changes.
func (c *Collection) Entry(id string) *Entry {
	if id == "" {
		return nil
	}
	for i := range c.Manifest.Tracks {
		if e := &c.Manifest.Tracks[i]; e.RequestedID == id || e.PlayedID == id {
			return e
		}
	}
	return nil
}

// Ensure returns the entry for t, filling metadata it lacks, or appends a
// pending entry for t at the next position.
func (c *Collection) Ensure(t Track) *Entry {
	if e := c.match(t); e != nil {
		e.merge(t, false)
		return e
	}
	next := 1
	for _, e := range c.Manifest.Tracks {
		next = max(next, e.Position+1)
	}
	e := newEntry(t)
	e.Position = next
	c.Manifest.Tracks = append(c.Manifest.Tracks, e)
	return &c.Manifest.Tracks[len(c.Manifest.Tracks)-1]
}

// Sync orders the manifest after tracks: positions run 1..n in that order,
// new tracks are added as pending, and existing ones keep their status with
// refreshed metadata. When complete is true the list is the whole
// collection, so entries it lacks are marked removed (their files stay);
// otherwise they keep their status. Either way they move after the listed
// tracks. A track listed twice keeps its first position.
func (c *Collection) Sync(tracks []Track, complete bool) {
	old := c.Manifest.Tracks
	used := make([]bool, len(old))
	find := func(t Track) int {
		for i := range old {
			if !used[i] && old[i].matches(t) {
				return i
			}
		}
		return -1
	}

	out := make([]Entry, 0, max(len(old), len(tracks)))
	listed := map[string]bool{}
	for _, t := range tracks {
		if t.ID == "" || listed[t.ID] {
			continue
		}
		listed[t.ID] = true
		var e Entry
		if i := find(t); i >= 0 {
			used[i] = true
			e = old[i]
			e.merge(t, true)
			if e.Status == StatusRemoved {
				e.Status = e.restoredStatus()
			}
		} else {
			e = newEntry(t)
		}
		e.Position = len(out) + 1
		out = append(out, e)
	}
	for i, e := range old {
		if used[i] {
			continue
		}
		if complete && len(tracks) > 0 { // an empty list is never trusted to remove everything
			e.Status = StatusRemoved
		}
		e.Position = len(out) + 1
		out = append(out, e)
	}
	c.Manifest.Tracks = out
}

// Path returns the absolute path of e's file, or "" when it has none.
func (c *Collection) Path(e *Entry) string {
	if e == nil || e.File == "" {
		return ""
	}
	return filepath.Join(c.Dir, e.File)
}

// Done reports whether the song with id is complete in this folder: its
// entry is recorded or copied (not a kept partial) and the file exists.
func (c *Collection) Done(id string) bool {
	e := c.Entry(id)
	return e != nil && e.complete() && exists(c.Path(e))
}

// match finds the entry for t by its requested or played ID.
func (c *Collection) match(t Track) *Entry {
	if e := c.Entry(t.ID); e != nil {
		return e
	}
	return c.Entry(t.PlayedID)
}

// refresh copies a spec's descriptive fields into the manifest and reports
// whether anything changed.
func (c *Collection) refresh(s Spec) bool {
	m := &c.Manifest
	changed := false
	set := func(dst *string, v string) {
		if v != "" && *dst != v {
			*dst, changed = v, true
		}
	}
	set(&m.Name, s.Name)
	set(&m.Owner, s.Owner)
	set(&m.URL, s.URL)
	return changed
}

// activeCount is the number of tracks still in the collection.
func (c *Collection) activeCount() int {
	n := 0
	for _, e := range c.Manifest.Tracks {
		if e.Status != StatusRemoved {
			n++
		}
	}
	return n
}

// fileName returns the file name e is stored under: its existing name, or
// a new one that no other track in the folder uses.
func (c *Collection) fileName(e *Entry) string {
	if e.File != "" {
		return e.File
	}
	name := trackFileName(c.Manifest.Kind, e.Position, e.Artist, e.Title)
	for _, candidate := range []string{name, withSuffix(name, shortID(e.RequestedID)), withSuffix(name, e.RequestedID)} {
		if !c.nameTaken(candidate, e) {
			return candidate
		}
	}
	return withSuffix(name, fmt.Sprintf("%s-%d", e.RequestedID, e.Position))
}

// nameTaken reports whether another entry uses name, or a file of that
// name exists that self does not own. Names compare case-insensitively, as
// APFS does by default.
func (c *Collection) nameTaken(name string, self *Entry) bool {
	for i := range c.Manifest.Tracks {
		if o := &c.Manifest.Tracks[i]; o != self && strings.EqualFold(o.File, name) {
			return true
		}
	}
	_, err := os.Stat(filepath.Join(c.Dir, name))
	return err == nil
}

// playlist renders the .m3u8 of the folder's done songs in order.
func (c *Collection) playlist() []byte {
	var b strings.Builder
	b.WriteString("#EXTM3U\n")
	for _, e := range c.Manifest.Tracks {
		if !e.Status.Done() || e.File == "" {
			continue
		}
		secs := int64(-1)
		switch {
		case e.DurationMS > 0:
			secs = int64(math.Round(float64(e.DurationMS) / 1000))
		case e.RecordedMS > 0:
			secs = int64(math.Round(float64(e.RecordedMS) / 1000))
		}
		label := e.Title
		if e.Artist != "" {
			label = e.Artist + " - " + e.Title
		}
		_, _ = fmt.Fprintf(&b, "#EXTINF:%d,%s\n%s\n", secs, strings.ReplaceAll(label, "\n", " "), e.File)
	}
	return []byte(b.String())
}

func (c *Collection) sortTracks() {
	slices.SortStableFunc(c.Manifest.Tracks, func(a, b Entry) int { return a.Position - b.Position })
}

// newEntry returns a pending entry for t.
func newEntry(t Track) Entry {
	var e Entry
	e.RequestedID = t.ID
	e.Status = StatusPending
	e.merge(t, true)
	return e
}

// merge copies t's metadata into e: every non-empty field when overwrite
// is set, otherwise only the fields e lacks.
func (e *Entry) merge(t Track, overwrite bool) {
	str := func(dst *string, v string) {
		if v != "" && (overwrite || *dst == "") {
			*dst = v
		}
	}
	num := func(dst *int, v int) {
		if v != 0 && (overwrite || *dst == 0) {
			*dst = v
		}
	}
	if t.PlayedID != "" && t.PlayedID != e.RequestedID {
		str(&e.PlayedID, t.PlayedID)
	}
	str(&e.Title, t.Title)
	str(&e.Artist, t.Artist)
	str(&e.Album, t.Album)
	str(&e.AlbumArtist, t.AlbumArtist)
	str(&e.ArtworkURL, t.ArtworkURL)
	num(&e.TrackNumber, t.TrackNumber)
	num(&e.DiscNumber, t.DiscNumber)
	num(&e.Popularity, t.Popularity)
	if t.DurationMS != 0 && (overwrite || e.DurationMS == 0) {
		e.DurationMS = t.DurationMS
	}
}

// matches reports whether e is the entry for t, comparing the requested
// and played IDs on both sides (Spotify relinks tracks between releases).
func (e *Entry) matches(t Track) bool {
	ids := []string{e.RequestedID, e.PlayedID}
	for _, id := range []string{t.ID, t.PlayedID} {
		if id != "" && slices.Contains(ids, id) {
			return true
		}
	}
	return false
}

// complete reports a done song that is not a kept partial.
func (e *Entry) complete() bool { return e.Status.Done() && !e.Partial }

// playedOrRequested is the ID of the recording itself.
func (e *Entry) playedOrRequested() string {
	if e.PlayedID != "" {
		return e.PlayedID
	}
	return e.RequestedID
}

// restoredStatus is the status a removed entry returns to when its track
// reappears in the collection.
func (e *Entry) restoredStatus() Status {
	switch {
	case e.File == "" || e.AudioSHA256 == "":
		return StatusPending
	case e.CopiedFrom != "":
		return StatusCopied
	}
	return StatusRecorded
}

// track converts an entry back to the metadata it was built from.
func (e *Entry) track() Track {
	return Track{
		ID: e.RequestedID, PlayedID: e.PlayedID,
		Title: e.Title, Artist: e.Artist, Album: e.Album, AlbumArtist: e.AlbumArtist,
		DurationMS: e.DurationMS, TrackNumber: e.TrackNumber, DiscNumber: e.DiscNumber,
		Popularity: e.Popularity, ArtworkURL: e.ArtworkURL,
	}
}
