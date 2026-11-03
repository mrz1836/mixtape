package library

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/mrz1836/mixtape/internal/fileutil"
	"github.com/mrz1836/mixtape/internal/mp4tag"
)

// reconcile adopts files that exist for entries the manifest does not yet
// count as done: the crash window between a file's rename into place and
// the manifest write. A file is adopted only when its own SPOTIFY_ID (or
// SPOTIFY_REQUESTED_ID) tag names the entry and its audio is intact. It
// changes c in memory and reports whether anything changed; Lock persists
// it.
func (l *Library) reconcile(c *Collection) bool {
	r := &reconciler{l: l, c: c, referenced: map[string]bool{}}
	open := r.index()
	if len(open) == 0 {
		return false
	}
	if rest := r.adoptOwnNames(open); len(rest) > 0 {
		r.adoptUnreferenced(rest)
	}
	return r.changed
}

// reconciler is the state of one reconcile of a collection.
type reconciler struct {
	l *Library
	c *Collection
	// referenced holds the lower-cased names done entries own, so a file
	// is never adopted twice (APFS compares names case-insensitively).
	referenced map[string]bool
	changed    bool
}

// index records the names the done entries own and returns the indexes of
// the entries that may still adopt a file: neither done nor removed.
func (r *reconciler) index() []int {
	var open []int
	for i, e := range r.c.Manifest.Tracks {
		switch {
		case e.Status.Done():
			if e.File != "" {
				r.referenced[strings.ToLower(e.File)] = true
			}
		case e.Status != StatusRemoved:
			open = append(open, i)
		}
	}
	return open
}

// adoptOwnNames is the first pass: each open entry adopts the file under
// the name it has or would get. It needs no folder listing, so it works in
// a folder that cannot be listed. It returns the entries still without a
// file.
func (r *reconciler) adoptOwnNames(open []int) []int {
	var rest []int
	for _, i := range open {
		e := &r.c.Manifest.Tracks[i]
		name := e.File
		if name == "" {
			name = trackFileName(r.c.Manifest.Kind, e.Position, e.Artist, e.Title)
		}
		if tags, err := mp4tag.ReadFile(filepath.Join(r.c.Dir, name)); err == nil && taggedFor(tags, e) && r.adopt(i, name, tags) {
			continue
		}
		rest = append(rest, i)
	}
	return rest
}

// adoptUnreferenced is the second pass: any .m4a in the folder that no
// done entry owns is adopted by the first of rest its tags name (a file
// renamed by hand, or saved under a collision suffix).
func (r *reconciler) adoptUnreferenced(rest []int) {
	files, err := os.ReadDir(r.c.Dir)
	if err != nil {
		return
	}
	for _, f := range files {
		name := f.Name()
		if f.IsDir() || !strings.EqualFold(filepath.Ext(name), m4aExt) || r.referenced[strings.ToLower(name)] || fileutil.IsTemp(name) {
			continue
		}
		tags, readErr := mp4tag.ReadFile(filepath.Join(r.c.Dir, name))
		if readErr != nil {
			continue
		}
		rest = r.claim(rest, name, tags)
	}
}

// claim adopts the file name for the first entry of rest its tags name and
// returns rest without that entry.
func (r *reconciler) claim(rest []int, name string, tags mp4tag.Tags) []int {
	for k, i := range rest {
		if taggedFor(tags, &r.c.Manifest.Tracks[i]) && r.adopt(i, name, tags) {
			return slices.Delete(rest, k, k+1)
		}
	}
	return rest
}

// adopt marks entry i recorded as the file name, whose tags name it, and
// reports whether it did.
func (r *reconciler) adopt(i int, name string, tags mp4tag.Tags) bool {
	c := r.c
	e := &c.Manifest.Tracks[i]
	// A file from the crash window always matches its own hash tag. One
	// that does not (or cannot be hashed) is a song verify sent back to
	// pending for changed or truncated audio: it must be recorded again,
	// not adopted on the next Open.
	hash, err := mp4tag.HashFile(filepath.Join(c.Dir, name))
	if err != nil {
		return false
	}
	if tagged := tags.Freeform[TagAudioSHA256]; tagged != "" && tagged != hash {
		return false
	}
	if e.PendingWAV != "" {
		c.staleWAVs = append(c.staleWAVs, e.PendingWAV) // Lock removes it: Open must not write
	}
	if played := tags.Freeform[TagSpotifyID]; played != "" && played != e.RequestedID {
		e.PlayedID = played // the relink the lost manifest write held
	}
	e.Status, e.File, e.AudioSHA256 = StatusRecorded, name, hash
	e.Partial = tags.Freeform[TagPartial] == "1"
	e.FailReason, e.PendingWAV = "", ""
	r.referenced[strings.ToLower(name)] = true
	r.changed = true
	r.l.warn("%s: found %s on disk; marked it recorded", c.Folder(), name)
	return true
}

// taggedFor reports whether a file's identity tags name entry e.
func taggedFor(tags mp4tag.Tags, e *Entry) bool {
	id, req := tags.Freeform[TagSpotifyID], tags.Freeform[TagRequestedID]
	return (id != "" && (id == e.RequestedID || id == e.PlayedID)) || (req != "" && req == e.RequestedID)
}

// cleanTemps removes temp files a crashed write left in c's folder.
func (l *Library) cleanTemps(c *Collection) {
	files, err := os.ReadDir(c.Dir)
	if err != nil {
		return
	}
	for _, f := range files {
		if f.IsDir() || !fileutil.IsTemp(f.Name()) {
			continue
		}
		if err := os.Remove(filepath.Join(c.Dir, f.Name())); err == nil {
			l.warn("%s: removed stale temp file %s", c.Folder(), f.Name())
		}
	}
}
